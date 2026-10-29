# DeployBundle

DeployBundle 是一个离线交付工具：把 Docker Compose、镜像归档和可选的项目内配置文件加密，使用签名 manifest 描述内容，交给目标主机验证并启动 Compose。

它解决的是交付内容的保密和完整性问题。运行端不把客户 Docker 主机当成必须与发布端完全一致的镜像基线系统。

## 运行模型

发布端执行：

```text
Compose + Docker 本地镜像
        │
        ▼
deploy-pack create
        │
        ▼
manifest.json + manifest.sig + 加密 artifact
```

目标主机执行：

```text
deploy-run inspect / verify / apply
        │
        ├─ 验证 manifest 签名和 artifact 哈希
        ├─ 解密并验证 Compose
        ├─ 有镜像 artifact 时执行 docker load
        └─ docker compose ... up -d --pull never
```

运行端不会比较客户机已有镜像的 `localImageId`，也不会使用 `baseManifestSha256` 拒绝更新。镜像是否能被 Compose 使用，由本地镜像标签和 Docker Compose 自身判断。

## Bundle 类型

- **完整 Bundle**：包含 Compose 和全部镜像。用于首次部署、完整回退或客户机没有可靠本地镜像的场景。
- **config-only 增量 Bundle**：不包含镜像 artifact，适合只改 Compose、环境变量或资产引用的更新。`config-only` 表示本次没有携带镜像，不限定变化一定只有环境变量。
- **mixed 增量 Bundle**：包含完整最终 Compose 和本次变化的镜像 artifact；未变化镜像不重复传输。

`--base-bundle` 和 `--base-manifest` 只在发布端用于筛选变化镜像和复用未变化资产。它们不会在客户机上形成镜像 ID 锁。增量 manifest 中的 `requiredImages` 是签名的打包和审计元数据，运行端不拿它比较客户 Docker 状态。

## 镜像标签

新 Bundle 默认把 Compose 的原始 `image` 引用作为运行时镜像引用，并固定写入：

```yaml
pull_policy: never
```

这样目标机不会因为更新而联网拉取镜像。有镜像更新时，随包的加密 archive 由 `docker load` 加载；只有配置更新时，Compose 使用目标机已有的本地标签。

基于旧 Bundle 生成配置-only 更新时，如果服务的原始引用和镜像内容没有变化，打包端会保留旧 Bundle 使用的标签，以兼容已经加载的历史 Bundle。旧标签不会因为升级 runner 自动迁移。

## 密钥和资产路径

Bundle 使用两个不同用途的密钥文件：

- `key.bin`：AES 对称加密密钥；
- `signing.pub`：目标机验签公钥；
- `signing.seed`：仅发布端使用的 Ed25519 私钥。

每个 Bundle 的 `key.bin` 和 `signing.pub` 内容必须与该 Bundle 的加密和签名信息匹配。路径可以保持不变，但更换密钥不是普通配置更新，需要用新密钥重新生成并交付 Bundle。

带资产的 Bundle 使用 `--assets-dir` 指定持久化资产根目录，默认值为：

```text
/var/lib/deploybundle/assets
```

如果增量 Bundle 的 manifest 包含 `assetsBaseManifestSha256`，它没有自己的资产 archive，`apply` 时必须在这个根目录下找到并通过校验对应的旧资产目录。`verify` 只验证随包携带的 artifact，不代表被引用的旧资产目录已经存在；资产引用是否可用由 `apply` 检查。

## 环境变量

环境变量在发布端固化到加密 Compose 中。`--env-file` 只用于打包阶段，目标机不需要原始 `.env` 或 `env_file` 文件：

```bash
./bin/deploybundle-pack-linux-amd64 create \
  --compose ./compose.yaml \
  --env-file ./production.env \
  --out ./release/prod-cpu \
  --key-file ./keys/key.bin \
  --signing-key ./keys/signing.seed \
  --bundle-id prod-cpu \
  --release 2026.10.29 \
  --platform linux/amd64
```

修改环境变量后重新生成 Bundle。持有 `key.bin` 的人可以解密 Compose；Docker root 或 daemon 管理员可以读取运行时环境。该工具不是 DRM。

## 生成 Bundle

先生成密钥：

```bash
./bin/deploybundle-pack-linux-amd64 keygen \
  --key-out ./keys/key.bin \
  --signing-private-out ./keys/signing.seed \
  --signing-public-out ./keys/signing.pub
```

生成完整 Bundle：

```bash
./bin/deploybundle-pack-linux-amd64 create \
  --compose ./compose.yaml \
  --out ./release/prod-cpu-full \
  --key-file ./keys/key.bin \
  --signing-key ./keys/signing.seed \
  --bundle-id prod-cpu \
  --release 2026.10.29-full \
  --platform linux/amd64
```

基于上一版 manifest 生成增量 Bundle：

```bash
./bin/deploybundle-pack-linux-amd64 create \
  --compose ./compose.yaml \
  --out ./release/prod-cpu-2026.10.29 \
  --key-file ./keys/key.bin \
  --signing-key ./keys/signing.seed \
  --bundle-id prod-cpu \
  --release 2026.10.29 \
  --base-manifest ./release/prod-cpu-full/manifest.json \
  --platform linux/amd64
```

`--base-bundle` 读取完整旧 Bundle；`--base-manifest` 只读取旧 `manifest.json`。两者都只影响发布端筛选变化镜像，必须使用相同的 `bundle-id` 和目标平台。

如果只需要为另一套 Compose 环境重新加密配置，而镜像完全复用已有完整 Bundle，可以使用 `--reuse-images-from PATH` 生成独立完整 Bundle。该参数复用源完整 Bundle 的加密镜像 archive，并重新生成 Compose、manifest 和签名；它与 `--base-bundle`、`--base-manifest` 互斥。

如果需要加密打包 Compose 项目内的相对 bind、`configs.file` 或 `secrets.file` 文件，增加：

```bash
--package-assets
```

资产只接受项目目录内的普通文件，manifest 记录路径、哈希、大小和权限。绝对宿主机路径和 named volume 仍由目标主机提供。

## 验证和应用

检查清单摘要：

```bash
./bin/deploybundle-run-linux-amd64 inspect \
  --bundle ./release/prod-cpu-2026.10.29 \
  --verify-key ./keys/signing.pub
```

验证签名、artifact、解密流和宿主机预检：

```bash
./bin/deploybundle-run-linux-amd64 verify \
  --bundle ./release/prod-cpu-2026.10.29 \
  --key-file ./keys/key.bin \
  --verify-key ./keys/signing.pub
```

应用 Bundle：

```bash
./bin/deploybundle-run-linux-amd64 apply \
  --bundle ./release/prod-cpu-2026.10.29 \
  --key-file ./keys/key.bin \
  --verify-key ./keys/signing.pub \
  --assets-dir /var/lib/deploybundle/assets
```

`--service NAME` 可以重复，用于只启动已验证 Compose 模型中的指定服务。不传时保持 Compose 的全量启动语义。`--skip-host-preflight` 只适合受控测试；客户 GPU 部署应保留默认预检。

同一服务栈应保持稳定的 `bundle-id`，因为 runner 会根据它生成 Compose project 名。`bundle-id` 是项目身份，不是镜像标签或镜像 ID。

## CPU/GPU 更新顺序

跨主机 GPU 部署时，通常先应用 CPU Bundle，再应用 GPU Bundle。每个 Bundle 都要单独执行 `verify` 和 `apply`。如果是 config-only 更新，不会传输或加载镜像；如果是 mixed 更新，只加载本次 artifact 中的镜像。

## 回滚

回滚就是用相同的 runner 重新 `apply` 目标版本 Bundle：

```bash
./bin/deploybundle-run-linux-amd64 verify \
  --bundle ./release/prod-cpu-previous \
  --key-file ./keys/key.bin \
  --verify-key ./keys/signing.pub

./bin/deploybundle-run-linux-amd64 apply \
  --bundle ./release/prod-cpu-previous \
  --key-file ./keys/key.bin \
  --verify-key ./keys/signing.pub \
  --assets-dir /var/lib/deploybundle/assets
```

回滚能恢复目标 Bundle 中的 Compose、它携带的镜像和它携带或引用的资产，具体取决于该 Bundle 的内容：

- 旧完整 Bundle 携带全部镜像，适合作为完整镜像回退包；
- 旧 config-only Bundle 不携带镜像，不能单独恢复后来被覆盖的镜像内容；
- 旧 mixed Bundle 只能恢复它实际携带的变化镜像；
- 引用旧资产的 Bundle 仍要求对应资产目录存在。

回滚不会恢复数据库、Docker volume 或其他持久化数据，也不会自动执行 `down`、`prune` 或清理其他项目。Compose 启动不是事务，失败后可能留下部分容器状态。

## Bundle 内容和安全边界

典型 Bundle：

```text
release/prod-cpu-2026.10.29/
├── manifest.json
├── manifest.sig
├── compose.enc
├── images.tar.gzip.enc    # 完整包或 mixed 增量包可选
└── assets.tar.gzip.enc    # 可选
```

manifest 签名覆盖 Bundle 描述、Compose 摘要、镜像元数据、资产元数据、目标平台和宿主机能力摘要。AES-GCM 加密流和 artifact SHA-256 用于发现密文替换、截断和篡改。

`requiredImages`、`baseManifestSha256` 和 `localImageId` 可以存在于增量 manifest 中，但它们是结构、审计和打包筛选信息，不是客户机镜像状态锁。`assetsBaseManifestSha256` 不同：它表示资产不在本包中，`apply` 必须从 `--assets-dir` 找到并校验对应资产目录。

当前支持 schema 1、2、3：schema 1 为无资产完整包，schema 2 为带完整资产 artifact 的完整包，schema 3 为增量包。manifest 结构、签名、公钥和加密流版本仍会严格校验。

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

集成测试覆盖完整 Bundle、config-only 和 mixed 增量包、环境变量固化、资产加密、legacy 标签兼容、签名和 artifact 篡改，以及 apply 阶段不依赖客户机镜像 ID 检查。

## 当前限制

- 每个 Bundle 只对应一个目标平台；
- 服务必须已有 `image`，不支持只有 `build` 的服务；
- 不自动拉取私有 registry 镜像；
- 不提供数据库、volume 或业务数据回滚；
- 不提供 GPU 驱动、端口冲突和完整权限策略的自动诊断；
- 不支持外部 secrets/configs 的独立密钥管理；
- Docker root 或 daemon 管理员可以读取运行时内容。

不要把 `key.bin`、`signing.seed`、访问令牌或其他私密材料提交到 Git、Bundle 或日志中。

## 许可证

本项目采用 [Apache License 2.0](./LICENSE)。
