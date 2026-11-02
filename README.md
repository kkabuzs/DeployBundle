# DeployBundle

DeployBundle 加密 Docker Compose 和可选的项目内配置文件，使用签名 manifest 描述内容，在目标主机验证后交给 Docker Compose 启动。镜像使用普通 Docker save 归档或由目标机拉取，不再生成加密镜像包。

每次生成的都是独立配置包，不要求旧 Bundle 或基线 manifest。工具不比较客户机镜像 ID，也不维护发布版本状态。

## 运行模型

发布端执行：

```text
本地完整 Compose + 可选配置资产
        ↓
deploy-pack create
        ↓
manifest.json + manifest.sig + compose.enc + 可选资产密文
```

目标主机默认执行：

```text
验证签名、artifact 哈希和加密流
        ↓
解密 Compose、准备资产和主机预检
        ↓
docker compose ... up -d --pull never --no-build
```

默认 `apply` 不 load、不 pull、不 build，即使目录中存在 `images.tar` 或旧加密镜像 archive；本地缺少所需镜像时由 Compose 报错。

显式指定 `--update-images` 才准备镜像：

```text
存在 images.tar？
    ├─ 有：docker load，失败则停止
    └─ 无：旧包含加密镜像 archive？
              ├─ 有：按旧格式验证、解密并 load，失败则停止
              └─ 无：docker compose pull --policy always
                       └─ 失败时警告，尝试使用已有本地镜像启动
        ↓
docker compose ... up -d --pull never --no-build
```

pull 失败后本地启动成功，不表示镜像已经更新；runner 会明确提示镜像更新未确认。本地仍缺少所需镜像时启动失败。load 读取整个归档，不按 `--service` 筛选归档内容。

## Compose 和项目身份

打包端保留输入 Compose 的原始 `image`、`pull_policy`、显式卷名、网络、端口和挂载，不生成 `bundle.local/...` 标签，也不注入 `pull_policy: never`。运行端仅在实际启动命令中禁止再次拉取和构建，镜像准备由 `--update-images` 控制。

同一服务栈应保持稳定的 `bundle-id`，因为 runner 会根据它生成带 hash 后缀的 Compose project 名。它是项目身份，不是镜像基线。已部署的数据卷和显式网络名也应保持正确，例如：

```yaml
volumes:
  database_data:
    name: existing_database_data
```

配置包包含完整 Compose；改变顶层卷名也会改变实际挂载，可能触发数据库容器重建并改挂新卷。工具不会自动继承旧 Compose 中未写入本次输入文件的字段。

## 密钥和资产路径

- `key.bin`：AES 对称加密密钥；
- `signing.pub`：目标机验签公钥；
- `signing.seed`：仅发布端使用的 Ed25519 签名私钥。

密钥内容必须与目标 Bundle 匹配。路径相同不代表任意密钥都兼容。持有 `key.bin` 的人可以解密 Compose；Docker root 或 daemon 管理员可以读取运行时内容。

使用 `--package-assets` 时，Compose 项目内相对 bind、`configs.file` 和 `secrets.file` 的普通文件会随包加密。绝对宿主机路径和 named volume 仍由目标机提供。新包自带完整所需资产，不依赖上一版资产摘要。

`apply --assets-dir` 指定资产根目录，默认 `/var/lib/deploybundle/assets`。新配置包的资产目录按项目身份和资产内容确定；相同内容跨 release 使用相同路径，环境变量或 release 变化不会单独改变资产挂载路径。文件内容、路径或权限变化会产生新的内容目录。目录和文件仍会进行完整性校验。

旧包的 manifest 若含 `assetsBaseManifestSha256`，仍按旧规则在该资产根目录查找并校验旧资产目录。`verify` 不确认被引用目录已经存在，由 `apply` 检查。

## 生成配置包

先生成密钥：

```bash
./bin/deploybundle-pack-linux-amd64 keygen \
  --key-out ./keys/key.bin \
  --signing-private-out ./keys/signing.seed \
  --signing-public-out ./keys/signing.pub
```

直接使用本地维护的完整 Compose 生成配置包：

```bash
./bin/deploybundle-pack-linux-amd64 create \
  --compose ./compose.yaml \
  --out ./release/prod-cpu-2026.11.02 \
  --key-file ./keys/key.bin \
  --signing-key ./keys/signing.seed \
  --bundle-id prod-cpu \
  --release 2026.11.02 \
  --platform linux/amd64 \
  --package-assets
```

没有相对资产时省略 `--package-assets`。打包只需要 Docker Compose CLI 解析配置，不要求 Docker daemon 已有服务镜像，不检查镜像 OS/架构或访问 registry。目标平台、MAC binding 和宿主机资源的原有预检仍保留。

`--env-file PATH` 可选：Compose 在打包时解析 shell 变量、`.env` 和服务 `env_file`，最终环境变量固化到密文中。目标机不需要原始环境文件。没有环境文件时不传该参数。

`--base-bundle`、`--base-manifest` 和 `--reuse-images-from` 已从新制包流程移除，传入时会明确报错。新包不会创建镜像 artifact 或基线引用。

## 验证和应用

可单独检查摘要，或手动完整验证 Bundle：

```bash
./bin/deploybundle-run-linux-amd64 inspect \
  --bundle ./release/prod-cpu-2026.11.02 \
  --verify-key ./keys/signing.pub

./bin/deploybundle-run-linux-amd64 verify \
  --bundle ./release/prod-cpu-2026.11.02 \
  --key-file ./keys/key.bin \
  --verify-key ./keys/signing.pub
```

`verify` 独立执行完整校验，包括旧包声明的加密镜像 archive；它不受 `--update-images` 控制。日常配置更新直接使用 `apply` 即可：`apply` 会验证实际使用的配置和资产，仅在明确更新镜像且选择旧加密归档时才读取该归档。

默认只应用配置，使用已有本地镜像：

```bash
./bin/deploybundle-run-linux-amd64 apply \
  --bundle ./release/prod-cpu-2026.11.02 \
  --key-file ./keys/key.bin \
  --verify-key ./keys/signing.pub \
  --assets-dir /var/lib/deploybundle/assets
```

需要更新镜像或首次部署时，在同一条 `apply` 命令末尾增加 `--update-images`。这个参数只适用于 `apply`，不传给 `verify`。

需要离线携带镜像时，在发布端自行生成普通 save 包并放入配置包目录：

```bash
docker save -o ./release/prod-cpu-2026.11.02/images.tar \
  registry.example.com/app:2026.11.02
```

客户使用 `apply --update-images` 后自动 load。归档可以只带变化镜像，其他服务使用已有本地镜像；缺少其他所需镜像时仍会报错。没有 `images.tar` 时尝试 pull，客户需具备对应 registry 的网络和认证条件。

`--service NAME` 可重复指定，保持 Compose 原生服务选择和依赖处理；不传时按完整 Compose 更新。`--skip-host-preflight` 仅适合受控测试，客户 GPU 部署应保留默认预检。跨主机 CPU/GPU 服务栈通常先 CPU、再 GPU，各自调用 apply 验证并应用。

## 回滚

配置回滚是重新应用旧配置包，保持同一服务栈的 `bundle-id` 和数据卷映射：

```bash
./bin/deploybundle-run-linux-amd64 apply \
  --bundle ./release/prod-cpu-previous \
  --key-file ./keys/key.bin \
  --verify-key ./keys/signing.pub \
  --assets-dir /var/lib/deploybundle/assets
```

镜像回滚另加 `--update-images`，并准备旧 `images.tar`、可拉取的旧镜像标签，或兼容旧包中的加密镜像 archive。纯配置回滚不会恢复后来被覆盖的镜像内容。

回滚不恢复数据库、Docker volume 或其他持久化数据，也不会自动执行 `down`、`prune` 或清理其他项目。Compose 启动不是事务，失败后可能留下部分容器状态。

## Bundle 内容和安全边界

```text
release/prod-cpu-2026.11.02/
├── manifest.json
├── manifest.sig
├── compose.enc
├── assets.tar.gzip.enc    # 可选，签名描述的加密资产
└── images.tar             # 可选，外部提供的普通 Docker save 归档
```

签名 manifest 和 AES-GCM 加密流保护 Compose 及清单中声明的资产。普通 `images.tar` 不加密，也不在配置包签名覆盖范围内；`verify` 不认证这个外部镜像归档。

新配置包使用 schema 1（无资产）或 schema 2（自带资产），`bundleType=full`、`updateKind=config-only`，不包含镜像身份或基线信息。旧 schema 1/2 镜像包和 schema 3 增量包保留读取兼容；旧 `requiredImages`、`baseManifestSha256`、`localImageId` 不作为运行时镜像锁。新配置包需要使用本轮新版 runner。

## Shell 快捷脚本

- `scripts/deploybundle-create.sh`：填写 Compose、输出目录、密钥、项目身份和目标平台，生成独立配置包并 inspect；不要求基线或本地镜像。
- `scripts/deploybundle-apply.sh`：直接调用 apply，由 apply 完成所需校验。`UPDATE_IMAGES="false"` 为默认值；只有改为 `true` 才给 apply 增加 `--update-images`。独立的 verify 留作手动完整校验，不会被脚本无条件执行。

## 发布二进制

```text
bin/deploybundle-pack-linux-amd64
bin/deploybundle-pack-linux-arm64
bin/deploybundle-run-linux-amd64
bin/deploybundle-run-linux-arm64
bin/SHA256SUMS
```

构建并校验：

```bash
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
shasum -a 256 -c bin/SHA256SUMS
```

## 测试

```bash
GOCACHE=/tmp/deploybundle-gocache go test -count=1 -race ./...
GOCACHE=/tmp/deploybundle-gocache go vet ./...
git diff --check
```

测试覆盖独立配置包、环境变量固化、Compose 字段保留、资产加密与路径稳定、默认不操作镜像、普通 save 归档、旧镜像包兼容、pull 失败本地兜底，以及签名和 artifact 篡改。

## 当前限制

- 每个 Bundle 只对应一个目标平台；
- 服务必须已有 `image`，不支持只有 `build` 的服务；
- 不管理 registry 凭据；显式更新镜像时使用目标机已有的 Docker 认证；
- 不提供数据库、volume 或业务数据回滚；
- 不提供 GPU 驱动、端口冲突和完整权限策略的自动诊断；
- 不支持外部 secrets/configs 的独立密钥管理；
- Docker root 或 daemon 管理员可以读取运行时内容。

不要把 `key.bin`、`signing.seed`、访问令牌或其他私密材料提交到 Git、Bundle 或日志中。

## 许可证

本项目采用 [Apache License 2.0](./LICENSE)。
