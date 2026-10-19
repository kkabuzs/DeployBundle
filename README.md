# DeployBundle

DeployBundle 是一个与业务项目无关的 Docker Compose 离线交付工具。它从 Compose 文件读取服务镜像，把多个本地镜像导出到一个 bundle，使用 AES-256-GCM 分块加密，并用 Ed25519 签名 manifest。目标主机上的 `deploy-run` 会先验证签名、哈希和加密流，再加载镜像并启动 Compose。

## 功能

- 从 `services.*.image` 自动发现服务镜像。
- 检查镜像的 OS 和 CPU 架构，并支持一次打包多个镜像。
- 使用一次 `docker save` 生成镜像归档，支持 `gzip`（默认）或不压缩。
- 使用 `AES-256-GCM-chunked-v2` 分块加密；`verify` 流式解密不写入明文文件，`apply` 只在完整认证后写入权限为 0600 的临时归档。
- 使用 Ed25519 签名 manifest，manifest 同时记录 Compose 和加密 artifact 的 SHA-256。
- `inspect`、`verify`、`apply` 分阶段执行；`apply` 在完整认证镜像归档后才调用 `docker load`。
- 对目标平台、设备文件和绝对宿主机路径执行预检。
- 可选使用带随机 salt 的 MAC 地址绑定。

## 工作流程

```text
Compose + 本地 Docker images
             │
             ▼
      deploy-pack create
             │
             ▼
 manifest.json + manifest.sig + encrypted artifacts
             │
             ▼
      deploy-run inspect / verify / apply
             │
             ▼
   verified docker load + docker compose up -d
```

## 环境要求

打包端和部署端都需要：

- Docker CLI；
- Docker Compose plugin；
- 对应 Docker daemon 的访问权限。

`deploy-pack create` 需要 Compose plugin 和 Docker daemon，因为它会解析 Compose、检查镜像并执行 `docker save`。`deploy-run inspect` 和 `deploy-run verify` 只读取 bundle、验证密钥和执行本地预检，不调用 Docker daemon；`deploy-run apply` 需要 Docker daemon。

从源码构建需要 Go 1.27 或更高版本。当前发布目标为 Linux `amd64` 和 `arm64`。每个 bundle 只对应一个目标平台，打包端必须使用与目标 Docker daemon 一致的镜像和 `--platform`。

## 快速开始

下面的示例以 Linux ARM64 为例。先准备一个最小 Compose 文件，并确保镜像已经存在于打包端的 Docker daemon：

```bash
mkdir -p demo-work
cat > demo-work/compose.yaml <<'YAML'
services:
  web:
    image: nginx:1.30.5
    ports:
      - "8080:80"
YAML

docker pull nginx:1.30.5
```

准备密钥并创建 bundle。`--out` 指向的目录必须不存在：

```bash
PACK=./bin/deploybundle-pack-linux-arm64
RUN=./bin/deploybundle-run-linux-arm64

"$PACK" keygen \
  --key-out demo-work/key.bin \
  --signing-private-out demo-work/signing.seed \
  --signing-public-out demo-work/signing.pub

"$PACK" create \
  --compose demo-work/compose.yaml \
  --out demo-work/release/demo-linux-arm64 \
  --key-file demo-work/key.bin \
  --signing-key demo-work/signing.seed \
  --bundle-id demo-linux-arm64 \
  --release 2026.09.29 \
  --profile host-integrated-v1 \
  --platform linux/arm64
```

在目标 ARM64 主机上验证并部署：

```bash
"$RUN" inspect \
  --bundle demo-work/release/demo-linux-arm64 \
  --verify-key demo-work/signing.pub

"$RUN" verify \
  --bundle demo-work/release/demo-linux-arm64 \
  --key-file demo-work/key.bin \
  --verify-key demo-work/signing.pub

"$RUN" apply \
  --bundle demo-work/release/demo-linux-arm64 \
  --key-file demo-work/key.bin \
  --verify-key demo-work/signing.pub

curl --fail http://127.0.0.1:8080/
```

`create` 不会自动拉取私有 registry 镜像，也不支持只有 `build` 没有 `image` 的服务。打包后的 Compose 会固定 `pull_policy: never`，部署时不会重新拉取镜像。`apply` 启动 Compose 后不会等待 HTTP 或 healthcheck 就绪，应用可用性需要由调用方检查。

## 环境变量处理

当前采用打包时固化环境变量的方式。`deploy-pack create` 调用 Docker Compose 的 `config --format json --no-normalize`，由打包端 Compose 解析 shell 环境变量、项目 `.env`、变量插值和服务 `env_file`。解析后的完整 Compose 会进入 `compose.enc`，部署主机不需要原始 `.env` 或 `env_file`。

例如：

```yaml
services:
  app:
    image: example/app:1.0
    env_file:
      - ./production.env
    environment:
      APP_MODE: "${APP_MODE:?APP_MODE is required}"
      DB_PASSWORD: "${DB_PASSWORD:?DB_PASSWORD is required}"
```

打包前准备变量：

```bash
set -a
. ./production.env
set +a

./bin/deploybundle-pack-linux-amd64 create \
  --compose ./compose.yaml \
  --out ./release/prod-linux-amd64 \
  --key-file ./key.bin \
  --signing-key ./signing.seed \
  --bundle-id prod-linux-amd64 \
  --platform linux/amd64
```

`${VAR:?message}` 可让缺失变量在打包阶段直接失败。变量值会被加密保存在 `compose.enc` 中，并在 `apply` 时随已验证的 Compose 传给 Docker Compose。修改变量后需要重新生成 bundle；当前 CLI 没有单独的部署时 `--env-file` 注入模式。

环境变量在 bundle 中是加密的，但持有对称密钥的人可以解密，Docker root 或 daemon 管理员也可以在容器启动后读取运行时环境。生产环境应限制 `production.env`、`key.bin` 和签名私钥的权限，并避免把它们提交到 Git。

## Bundle 内容

```text
release/demo-linux-arm64/
├── manifest.json
├── manifest.sig
├── compose.enc
└── images.tar.gzip.enc
```

`manifest.json` 包含：

- bundle ID、发行版本、运行时 profile 和目标平台；
- 原始 Compose 与规范化 Compose 的 SHA-256；
- 每个服务的原始镜像引用、镜像 ID、rootfs layer ID 和 bundle 内本地 tag；
- GPU、设备文件和宿主机路径摘要；
- 加密算法、压缩方式、chunk size 和加密 artifact 哈希；
- 签名算法和签名 key ID。

manifest 使用规范化 JSON 签名。`manifest.sig`、`compose.enc` 和镜像 artifact 被替换或截断时，`verify` 和 `apply` 都会失败。

## 双架构发布

发布物名称遵循：

```text
<project>-<command>-<os>-<arch>
```

当前项目的发布物为：

```text
bin/
├── deploybundle-pack-linux-amd64
├── deploybundle-pack-linux-arm64
├── deploybundle-run-linux-amd64
├── deploybundle-run-linux-arm64
└── SHA256SUMS
```

在任意支持 Go 交叉编译的平台上构建：

```bash
mkdir -p bin
for arch in amd64 arm64; do
  GOOS=linux GOARCH="$arch" CGO_ENABLED=0 \
    GOCACHE=/tmp/deploybundle-gocache \
    go build -trimpath -buildvcs=false -ldflags='-s -w' \
    -o "bin/deploybundle-pack-linux-$arch" ./cmd/deploy-pack

  GOOS=linux GOARCH="$arch" CGO_ENABLED=0 \
    GOCACHE=/tmp/deploybundle-gocache \
    go build -trimpath -buildvcs=false -ldflags='-s -w' \
    -o "bin/deploybundle-run-linux-$arch" ./cmd/deploy-run
done

shasum -a 256 bin/deploybundle-*-linux-* > bin/SHA256SUMS
```

macOS 可使用 `shasum -a 256 -c bin/SHA256SUMS` 校验；Linux 可使用 `sha256sum -c bin/SHA256SUMS`。发布时应同时发布校验文件。

两个架构使用不同的原生二进制和不同的 bundle；单个二进制不会同时变成 amd64/arm64 可执行文件。`--platform` 只声明并检查目标平台，不会自动选择多架构镜像。例如可以为两个 Docker context 分别打包：

```bash
DOCKER_CONTEXT=prod-amd64 ./bin/deploybundle-pack-linux-amd64 create \
  --compose compose.yaml --out release/prod-linux-amd64 \
  --key-file key.bin --signing-key signing.seed \
  --bundle-id prod-linux-amd64 --platform linux/amd64

DOCKER_CONTEXT=prod-arm64 ./bin/deploybundle-pack-linux-arm64 create \
  --compose compose.yaml --out release/prod-linux-arm64 \
  --key-file key.bin --signing-key signing.seed \
  --bundle-id prod-linux-arm64 --platform linux/arm64
```

部署时使用与主机架构匹配的 `deploy-run` 和 bundle。`--allow-platform-mismatch` 只应在已确认 Docker 实际目标平台匹配时使用。

## CLI 参数要点

`deploy-pack create` 的常用参数：

| 参数 | 说明 |
| --- | --- |
| `--compose` | 输入 Compose 文件；每个服务必须已有 `image` |
| `--out` | 新的 bundle 目录；已存在时命令拒绝覆盖 |
| `--key-file` / `--signing-key` | AES 对称密钥和 Ed25519 私钥 |
| `--platform` | 目标 OS/架构，例如 `linux/amd64`；默认取当前 Go 运行时 |
| `--compression` | `gzip` 或 `none`，默认 `gzip` |
| `--chunk-size` | 加密 chunk 大小，默认 1 MiB，允许 4 KiB 至 64 MiB |
| `--docker` | Docker CLI 可执行文件路径，默认 `docker` |
| `--project-name` | 传给 Compose 的项目名 |
| `--bind-mac` | 可选，逗号分隔的部署主机 MAC 地址 |

`deploy-run verify/apply` 还支持 `--docker`、`--allow-platform-mismatch` 和 `--skip-host-preflight`。`--skip-host-preflight` 只适合受控测试，不能替代目标主机检查。

密钥文件包含敏感材料，`keygen` 会覆盖同名输出文件；请在安全目录中生成，并避免提交到 Git。

## Compose 与宿主机资源

MVP 不重写 Compose 的运行语义，只替换已打包服务的 `image` 并加入 `pull_policy: never`。以下字段会保留在规范化 Compose 中，并在 manifest 中形成摘要：

- `deploy.resources.reservations.devices`；
- `devices`；
- bind mount；
- `network_mode: host`；
- `pid: host`；
- `privileged`。

当前 `host-integrated-v1` 预检只确认声明的设备文件和绝对宿主机路径存在，不验证 GPU 驱动、权限、端口冲突或更细的危险路径策略。`apply` 失败后不会自动卸载已加载镜像，也没有自动回滚；需要由部署系统决定清理和重试策略。

源 Compose 的 `--project-name` 不会原样决定部署项目名。runner 会根据 bundle ID 生成带 hash 后缀的 Compose project 名，以降低不同 bundle 之间的名称冲突。

## 加密与安全边界

- 加密格式为 `AES-256-GCM-chunked-v2`，流头魔数为 `DBAE2`；每条流使用随机 96 位前缀，并为每个 frame 派生 nonce，终止帧也经过认证。
- Ed25519 签名覆盖规范化 manifest；artifact SHA-256 用于发现文件替换和截断。
- 当前 runner 拒绝旧 v1 加密流；旧 bundle 需要重新打包。
- MAC 绑定保存的是带随机 salt 的哈希，只是额外的部署条件，不会改变 Docker 管理员的读取能力。
- 持有对称密钥的人可以离线解密；Docker root 或 daemon 管理员可以在运行时读取镜像和 Compose。该工具不是 DRM。
- 当前 MVP 没有过期、撤销、外部 secrets/configs、密钥轮换或 registry 凭据管理能力。

不要把 `key.bin`、`signing.seed` 或其他私钥材料提交到 Git、bundle 或日志中。

## 测试与验证

运行单元测试、集成测试和竞态检测：

```bash
GOCACHE=/tmp/deploybundle-gocache go test -count=1 -race ./...
GOCACHE=/tmp/deploybundle-gocache go vet ./...
```

测试包含加密流截断、nonce/frame 大小、manifest 签名校验、artifact 哈希、平台预检、Compose project 名冲突、环境变量快照和 fake Docker CLI 端到端流程。

已完成的真实环境验证：

- Multipass VM 名称：`gitlab`；Ubuntu 24.04、Linux ARM64、Docker Engine 29.7.1、Docker Compose v5.4.0；
- `nginx:1.30.5` 单镜像 bundle 完成 `create`、`inspect`、`verify`、`apply`，随后手动请求返回 HTTP 200；
- `nginx:1.30.5` 与 `pgsty/silo:RELEASE.2026-09-16T00-00-00Z` 双镜像 bundle 均成功 `docker load`，nginx 返回 HTTP 200，helper 容器退出码为 0；
- 测试产生的容器、网络、临时 tag、key、bundle 和测试目录已清理，VM 原有容器和基础镜像保留。

以上真实闭环覆盖 ARM64、默认 gzip 和普通 Compose 服务；没有覆盖 AMD64 实机运行、GPU、宿主机设备、bind mount、MAC 绑定、端口冲突和 `--compression none`。AMD64 发布物已完成交叉编译和 ELF 架构检查，仍需在 AMD64 VM 或 CI runner 中做运行时验证。

## 项目结构

```text
cmd/deploy-pack/   # keygen、create
cmd/deploy-run/    # inspect、verify、apply
internal/          # Compose、Docker、文件、密钥和宿主机绑定实现
pkg/bundle/        # manifest、规范化 JSON 和加密流格式
integration/       # fake Docker CLI 集成测试
```

## 当前限制与后续计划

当前不支持：

- 只有 `build` 没有 `image` 的 Compose 服务；
- 自动拉取私有 registry 镜像；
- 直接输入 OCI archive；
- 外部 secrets/configs、许可证撤销和密钥轮换；
- 完整 GPU 驱动、权限和端口冲突预检；
- 单个发布物同时兼容两种 CPU 架构。

后续可在保持现有 bundle 基本格式的前提下增加 OCI 输入、zstd、许可证签发、密钥轮换和更完整的宿主机能力检查。

## 贡献

欢迎提交 issue 和 pull request。修改 bundle 格式、CLI 参数或安全边界时，请同时更新：

1. `README.md` 和命令帮助；
2. 对应单元测试或集成测试；
3. 加密格式、manifest schema 或兼容性说明。

提交前运行：

```bash
gofmt -w $(rg --files -g '*.go' cmd internal pkg integration)
GOCACHE=/tmp/deploybundle-gocache go test -count=1 -race ./...
GOCACHE=/tmp/deploybundle-gocache go vet ./...
```

## 许可证

本项目采用 [Apache License 2.0](./LICENSE)。使用、修改和再分发本项目时，请遵守许可证中的版权、专利、NOTICE 和免责声明条款。
