# Codex 工作交接

更新时间：2026-10-29（Asia/Shanghai）
当前主机：本机 macOS；测试服务器 `root@101.200.219.174`
项目路径：`/Users/dp/project/DeployBundle`
当前分支：`main`

## 当前目标

DeployBundle 用于把 Docker Compose、镜像归档和可选的项目内资产加密交付到内网主机，并在目标机验证后启动 Compose。当前运行策略优先满足加密交付和增量传输，不把客户 Docker 主机当成必须与发布端镜像 ID 完全一致的系统。

## 当前有效设计

### 发布端

- 完整 Bundle 包含加密 Compose 和全部镜像。
- `config-only` Bundle 不携带镜像 artifact；它可以只改 Compose/环境变量，也可以包含资产引用变化。
- `mixed` Bundle 携带完整最终 Compose 和本次变化的镜像 artifact。
- `--base-bundle`、`--base-manifest` 只在发布端用于筛选变化镜像和复用未变化资产。
- `requiredImages`、`baseManifestSha256`、`localImageId` 保留在 manifest 中用于结构、审计和打包筛选。
- 新 Bundle 默认使用 Compose 原始 `image` 引用；基于旧 Bundle 且镜像内容和原始引用均未变化时，保留旧运行时标签以兼容 legacy Bundle。
- 所有打包 Compose 都固定写入 `pull_policy: never`。

### 目标机运行端

`deploy-run apply` 的实际流程是：

1. 读取并验证 manifest、签名和 artifact 哈希；
2. 解密并验证 Compose；
3. 有镜像 artifact 时完整解密到临时 archive；
4. 有资产时验证或发布资产目录；
5. 执行平台、MAC binding 和宿主机能力预检；
6. 有镜像 archive 时执行 `docker load`；
7. 执行 `docker compose ... up -d --pull never`。

运行端不执行 `docker image inspect`，不比较客户机已有镜像 ID，也不使用 `baseManifestSha256` 拒绝更新。标签不存在时由 Docker Compose 报错。

`assetsBaseManifestSha256` 是例外：它表示资产不在当前 Bundle 中。使用这种 Bundle 时，`apply` 必须在 `--assets-dir` 下找到并严格校验对应的旧资产目录。`verify` 只验证当前 Bundle 携带的 artifact，不验证被引用资产目录是否已经存在。

### 回滚边界

回滚通过同一个 runner 重新 `apply` 目标版本 Bundle 完成。回滚能恢复该 Bundle 中实际包含的 Compose、镜像和资产：

- 完整 Bundle 可携带全部镜像，适合作为完整镜像回退包；
- `config-only` Bundle 不携带镜像，不能单独恢复后来被覆盖的镜像内容；
- `mixed` Bundle 只能恢复它实际携带的变化镜像；
- 引用旧资产的 Bundle 仍要求对应资产目录存在。

回滚不会恢复数据库、Docker volume 或其他持久化数据，也不会自动执行 `docker compose down`、`prune` 或清理其他项目。Compose 启动不是事务，失败后可能留下部分容器状态。

## 当前路径和发布物

本机四个 Linux 发布二进制：

```text
/Users/dp/project/DeployBundle/bin/deploybundle-pack-linux-amd64
/Users/dp/project/DeployBundle/bin/deploybundle-pack-linux-arm64
/Users/dp/project/DeployBundle/bin/deploybundle-run-linux-amd64
/Users/dp/project/DeployBundle/bin/deploybundle-run-linux-arm64
```

当前本机 `bin/SHA256SUMS`：

```text
b51a2ee2ed1ea13265f3a1d0e6e9a88745aa1caedcb2ec88d7ad284dfb92706b  bin/deploybundle-pack-linux-amd64
ed7948877d8f5f2635d4792396b8861b2376bb1a95c44f1a1baa2c62da62e1e5  bin/deploybundle-pack-linux-arm64
735bd77d1bb9f2f8018853d66c54218dacca280bb19915bb96ec59ce7d115e32  bin/deploybundle-run-linux-amd64
a69a89c2d2c7925a3672bffbb004bc3178d33e336c11b41359f057e82fb8cffb  bin/deploybundle-run-linux-arm64
```

174 测试服务器已替换的 amd64 runner：

```text
/data/byd/clientdeploy-update-001/deploybundle-run-linux-amd64
```

远端 runner SHA-256：

```text
735bd77d1bb9f2f8018853d66c54218dacca280bb19915bb96ec59ce7d115e32
```

远端 runner 权限为 `700`。新的 pack 二进制和 arm64 二进制未上传到 174。

客户交付密钥路径约定：

```text
/data/byd/clientdeploy/keys/key.bin
/data/byd/clientdeploy/keys/signing.pub
```

CPU 资产根目录约定：

```text
/var/lib/deploybundle/assets
```

密钥路径保持不变不代表任意密钥都兼容；`key.bin` 和 `signing.pub` 的内容必须与目标 Bundle 匹配。

## 174 上已确认的 Bundle

更新目录：

```text
/data/byd/clientdeploy-update-001
```

CPU 增量包：

```text
/data/byd/clientdeploy-update-001/bundle/piloteye-cpu-internal-delta-001
```

- `bundleType=delta`
- `schemaVersion=3`
- `updateKind=config-only`
- `bundleId=piloteye-runtest-cpu`
- `images=0`
- `requiredImages=29`
- `assets=10`
- 使用 `assetsBaseManifestSha256`，没有资产 artifact

GPU 增量包：

```text
/data/byd/clientdeploy-update-001/bundle/piloteye-gpu-internal-delta-001
```

- `bundleType=delta`
- `schemaVersion=3`
- `updateKind=config-only`
- `bundleId=piloteye-runtest-gpu`
- `images=0`
- `requiredImages=2`
- 无资产、无镜像 artifact

两个增量包均已使用新 amd64 runner 完成 `inspect` 和 `verify --skip-host-preflight`。没有执行客户侧 `apply`，也没有重新传输镜像。

两个增量目录当前只包含：

```text
compose.enc
manifest.json
manifest.sig
```

## 客户机更新流程

客户机需要先放置新 amd64 runner，并确认 SHA-256 为 `735bd77d...115e32`。不需要传输镜像 archive。

CPU 更新顺序：

```bash
/data/byd/clientdeploy-update-001/deploybundle-run-linux-amd64 verify \
  --bundle /data/byd/clientdeploy-update-001/bundle/piloteye-cpu-internal-delta-001 \
  --key-file /data/byd/clientdeploy/keys/key.bin \
  --verify-key /data/byd/clientdeploy/keys/signing.pub

/data/byd/clientdeploy-update-001/deploybundle-run-linux-amd64 apply \
  --bundle /data/byd/clientdeploy-update-001/bundle/piloteye-cpu-internal-delta-001 \
  --key-file /data/byd/clientdeploy/keys/key.bin \
  --verify-key /data/byd/clientdeploy/keys/signing.pub \
  --assets-dir /var/lib/deploybundle/assets
```

CPU 成功后再执行 GPU：

```bash
/data/byd/clientdeploy-update-001/deploybundle-run-linux-amd64 verify \
  --bundle /data/byd/clientdeploy-update-001/bundle/piloteye-gpu-internal-delta-001 \
  --key-file /data/byd/clientdeploy/keys/key.bin \
  --verify-key /data/byd/clientdeploy/keys/signing.pub

/data/byd/clientdeploy-update-001/deploybundle-run-linux-amd64 apply \
  --bundle /data/byd/clientdeploy-update-001/bundle/piloteye-gpu-internal-delta-001 \
  --key-file /data/byd/clientdeploy/keys/key.bin \
  --verify-key /data/byd/clientdeploy/keys/signing.pub
```

客户 GPU 环境应保留默认宿主机预检，不要把 `--skip-host-preflight` 作为常规参数。

## 客户机回滚流程

回滚到客户旧完整 Bundle 时，先验证再应用：

```bash
/data/byd/clientdeploy-update-001/deploybundle-run-linux-amd64 verify \
  --bundle /data/byd/clientdeploy/bundle/piloteye-cpu-customer-001 \
  --key-file /data/byd/clientdeploy/keys/key.bin \
  --verify-key /data/byd/clientdeploy/keys/signing.pub

/data/byd/clientdeploy-update-001/deploybundle-run-linux-amd64 apply \
  --bundle /data/byd/clientdeploy/bundle/piloteye-cpu-customer-001 \
  --key-file /data/byd/clientdeploy/keys/key.bin \
  --verify-key /data/byd/clientdeploy/keys/signing.pub \
  --assets-dir /var/lib/deploybundle/assets
```

GPU 使用对应的 `piloteye-gpu-customer-001` Bundle。回滚前必须确认旧 Bundle 内的 Compose 环境变量仍适用于客户环境；回滚旧完整 Bundle 会在客户机本地加载其中的镜像，但不会联网传输镜像。

## 代码和验证状态

当前 DeployBundle 代码、测试、README、交接记录、辅助脚本和 Linux 发布二进制已分为三个本地提交；当前分支领先 `origin/main`。工作区剩余未跟踪文件属于并行或既有工作，不要执行 `git reset`、`git clean`、`checkout` 或批量覆盖，也不要把 `deploy-v1/`、`FINAL_REQUIREMENTS.md`、`semantic-review/`、临时文件加入本项目提交。

本次代码变更的有效验证：

```text
GOCACHE=/tmp/deploybundle-gocache go test -count=1 -race ./...   通过
GOCACHE=/tmp/deploybundle-gocache go vet ./...                  通过
git diff --check                                                通过
shasum -a 256 -c bin/SHA256SUMS                                  通过
```

测试覆盖完整 Bundle、config-only 和 mixed 增量包、环境变量固化、资产加密、legacy 标签兼容、签名和 artifact 篡改，以及 apply 阶段不依赖客户机镜像 ID 检查。

发布二进制为 Linux `amd64` 和 `arm64` 静态 ELF。当前 macOS 主机不直接执行 Linux ELF；命令行行为由源码测试和 fake Docker 集成测试覆盖。

## 当前未完成事项

- 尚未执行客户机 CPU/GPU `apply`；
- 客户机是否已经放置新 runner、旧 CPU 资产目录是否存在，需要在客户机执行前确认；
- 客户机实际 Docker 标签和 Compose 项目运行状态，需要由客户现场验证；
- 业务数据、数据库和 Docker volume 不属于 Bundle 的自动回滚范围。

## 敏感信息规则

交接记录不写入密码、API Key、Token、私钥内容、完整环境变量或部署主机认证信息。目标机 root 或 Docker daemon 管理员可以读取运行时 Compose、环境和镜像，不能把 DeployBundle 描述为绝对防提取方案。
