# Codex 工作交接

更新时间：2026-10-19
当前主机：本机 macOS
项目路径：`/Users/dp/project/DeployBundle`
当前分支：`main`，HEAD `6ff8bde`，已推送到 `origin/main`

## 当前目标

开发与项目无关的 `deploy-pack` / `deploy-run`，输入 Compose 和多个 Docker 镜像，生成签名加密交付包，在部署主机上校验、加载镜像并启动 Compose。

## 已确认事实

- 当前目录是 Git 仓库，远程为 `git@github.com:kkabuzs/DeployBundle.git`；代码和本交接文件位于 `/Users/dp/project/DeployBundle`。
- 上一对话的设计要求是通用工具，默认面向 `host-integrated-v1`，需要保留 GPU、宿主机设备和宿主机路径语义。
- 当前本机 Docker daemon 不可用，但 Docker Compose CLI v5.1.1 可执行。
- Multipass `gitlab` VM 为 Ubuntu 24.04 ARM64，Docker Engine 29.7.1、Compose v5.4.0；真实 Docker daemon 可用（VM 内需 `sudo`）。
- 已在 `gitlab` VM 用 `nginx:1.30.5` 完成一次真实 `docker save`、`docker load`、`docker compose up` 闭环；HTTP 请求返回 200，容器保持 running。
- 已在同一 VM 用 nginx 和 `pgsty/silo` 两个 ARM64 镜像完成真实打包、加载和 Compose 启动；nginx HTTP 返回 200，helper 容器退出码为 0。
- Docker Compose v5.4.0 的 `pull_policy`、镜像平台和规范化 JSON 在该最小 Compose 上通过。
- 已在用户提供的远程服务器 `101.200.219.174` 完成 Linux AMD64 真实验证：Docker Engine 28.1.1、Docker Compose v5.5.1；服务器现有 `docker.1ms.run/library/redis:7.2.6-alpine` 镜像为 Linux AMD64。
- 远程测试使用唯一 Compose project、独立网络、无宿主机端口映射、无宿主机挂载、无 host network 和无 privileged；测试前后现有容器、网络和卷的 ID/名称/镜像基线无差异。

## 基于证据的判断

- 使用 Docker Compose `config --format json` 可以避免在工具中重复实现 YAML 解析和 Compose 插值语义。
- 多个服务镜像由一次 `docker save` 合并成一个 archive，再 gzip 和 AES-256-GCM 分块加密；Compose 和镜像 artifact 分开加密。
- 当前加密流为 `AES-256-GCM-chunked-v2`（魔数 `DBAE2`）：每条流使用 96 位随机前缀，每帧使用派生的 96 位 nonce，终止帧带 GCM 认证；旧 v1 流需重新打包。
- 签名覆盖规范化 manifest；manifest 记录原始 Compose hash、规范化 Compose hash、镜像身份、目标平台及宿主机能力摘要。
- `apply` 在调用 Docker 前完整解密并校验镜像流到权限为 0600 的临时文件；Compose 使用已经校验过的内存内容。

## 尚未验证的可能性

- GPU、设备、宿主机路径和 MAC salted binding 尚未在目标 VM 复测；zstd、OCI archive 转换、许可证签发和密钥轮换尚未实现。

## 已完成

- 初始化 Go module `github.com/openai/deploybundle`。
- 实现 `pkg/bundle`：manifest v1、规范化 JSON、Ed25519 签名验证、AES-256-GCM-chunked-v2 分块流和 `DecryptWithInfo`。
- 实现 `deploy-pack keygen/create` 和 `deploy-run inspect/verify/apply`，以及可选的 salted MAC 绑定。
- manifest 现在严格校验加密、压缩和签名算法；runner 校验签名 keyId、流版本和 chunk size；打包时校验镜像 OS/architecture 与目标平台。
- `apply` 已改为先完整认证镜像 archive，再执行 `docker load`；Compose 读取验证后的内存内容；Compose project 名称带 bundle ID hash 后缀以避免碰撞。
- 新增 fake Docker CLI 端到端测试、宿主机设备/路径预检测试、artifact 路径和 project 名称测试。
- 在 Multipass `gitlab` VM 完成真实 ARM64 nginx 闭环：`deploy-pack create`、`deploy-run inspect`、`verify`、`apply`、HTTP 200 和容器状态检查；临时容器、网络、标签、key、bundle 和 VM 测试目录已清理。
- 在 Multipass `gitlab` VM 完成真实双镜像闭环：manifest 显示 2 个镜像，`docker load` 加载两个 local tag，Compose 启动 web/helper，HTTP 200，helper 退出码 0；测试资源已清理。
- 按 `<项目名>-<命令>-<系统>-<架构>` 规范构建四个 Linux 发布物：`deploybundle-pack/run-linux-amd64/arm64`，并生成 `bin/SHA256SUMS`；旧命名 ARM64 文件已移除。
- 按 GitHub 开源项目风格重写 `README.md`，补充快速开始、双架构发布、CLI 参数、验证边界、安全说明、贡献指南和许可证状态；文档中的发布物名称、校验命令和真实 VM 结果已与当前实现对齐。
- 添加标准 `LICENSE` 文件，项目许可证改为 Apache License 2.0，并在 README 中建立引用。
- 按方案 A 固化 Compose 环境变量：打包端通过 Docker Compose `config --format json --no-normalize` 解析 shell 变量、`.env`、变量插值和 `env_file`，解析后的 Compose 加密进入 `compose.enc`；README 已补充使用说明。
- 新增 `TestCLIEnvironmentSnapshot`，使用真实 Docker Compose CLI 验证 shell 变量和 `env_file` 值进入加密 Compose，并在 `apply` 时完整传递给 Compose。
- 在远程服务器 `/root/gotest` 完成 `keygen`、`create`、`inspect`、`verify`、`apply` 闭环；Redis 容器内 `redis-cli ping` 返回 `PONG`。
- 远程错误密钥验证和单字节篡改验证均按预期失败；测试容器、网络、匿名卷、临时镜像 tag、密钥、bundle 和测试目录已清理，`/root/gotest` 恢复为空。
- 远程 Docker Hub 的 `nginx:1.30.5` 拉取因网络超时失败，未留下 nginx 镜像；因此远程闭环使用服务器现有 Redis 镜像完成。
- 单元、竞态和静态检查均通过。
- 实现 Docker Compose JSON 解析、镜像检查/tag/save 流程和 host-integrated 能力摘要。

## 修改文件

- `go.mod`
- `pkg/bundle/manifest.go`
- `pkg/bundle/crypto.go`
- `pkg/bundle/crypto_test.go`
- `internal/compose/compose.go`
- `internal/docker/docker.go`
- `internal/files/files.go`
- `internal/keys/keys.go`
- `internal/binding/mac.go`
- `internal/binding/mac_test.go`
- `cmd/deploy-pack/main.go`
- `cmd/deploy-run/main.go`
- `cmd/deploy-run/main_test.go`
- `integration/cli_test.go`
- `pkg/bundle/manifest_test.go`
- `README.md`
- `LICENSE`
- `bin/deploybundle-pack-linux-amd64`
- `bin/deploybundle-pack-linux-arm64`
- `bin/deploybundle-run-linux-amd64`
- `bin/deploybundle-run-linux-arm64`
- `bin/SHA256SUMS`

## 验证结果

```text
GOCACHE=/tmp/deploybundle-gocache go test -count=1 -race ./...  通过
GOCACHE=/tmp/deploybundle-gocache go vet ./...                 通过
GOCACHE=/tmp/deploybundle-gocache go build -o /tmp/deploy-pack ./cmd/deploy-pack  通过
GOCACHE=/tmp/deploybundle-gocache go build -o /tmp/deploy-run ./cmd/deploy-run    通过
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 GOCACHE=/tmp/deploybundle-gocache go build -trimpath -buildvcs=false -ldflags='-s -w' -o bin/deploybundle-pack-linux-amd64 ./cmd/deploy-pack  通过
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 GOCACHE=/tmp/deploybundle-gocache go build -trimpath -buildvcs=false -ldflags='-s -w' -o bin/deploybundle-run-linux-amd64 ./cmd/deploy-run    通过
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 GOCACHE=/tmp/deploybundle-gocache go build -trimpath -buildvcs=false -ldflags='-s -w' -o bin/deploybundle-pack-linux-arm64 ./cmd/deploy-pack  通过
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 GOCACHE=/tmp/deploybundle-gocache go build -trimpath -buildvcs=false -ldflags='-s -w' -o bin/deploybundle-run-linux-arm64 ./cmd/deploy-run    通过
shasum -a 256 bin/deploybundle-*-linux-* > bin/SHA256SUMS  通过

VM（Multipass `gitlab`，Docker Engine 29.7.1 / Compose v5.4.0）：
`deploy-pack create`、`deploy-run inspect`、`deploy-run verify`、`deploy-run apply` 通过；VM 内 `curl http://127.0.0.1:18180/` 返回 nginx 页面（HTTP 200）。
双镜像 Compose（nginx + `pgsty/silo`）同样通过；helper 容器退出码为 0，原有 `gitlab`/`silo` 容器保持运行。

远程服务器（`101.200.219.174`，Linux AMD64，Docker Engine 28.1.1 / Compose v5.5.1）：
`deploy-pack keygen`、`create`、`deploy-run inspect`、`verify`、`apply` 通过；`docker load` 加载 bundle 内 Redis 镜像，独立 Compose project 启动成功，容器内 `redis-cli ping` 返回 `PONG`。
错误密钥和篡改后的镜像 artifact 均被 `verify` 拒绝；清理后测试容器、网络、卷和临时 tag 均不存在，`/root/gotest` 为空。

环境变量集成测试：`TestCLIEnvironmentSnapshot` 通过；shell 变量和 `env_file` 值在 `compose.enc` 解密后存在，密文不包含测试明文，`apply` 传给 Compose 的 JSON 保持一致。
```

## 未解决问题

- 需要在目标 VM 上复测 MAC 绑定、GPU/设备能力和包含宿主机资源的 Compose；当前真实 VM 测试未覆盖这些能力。
- 需要决定是否将 gzip 保留为标准库默认、将 zstd 作为可选外部工具，或引入纯 Go zstd 实现；OCI、许可证签发和密钥轮换仍未实现。

## 下一步

1. 在 `gitlab` VM 用包含 GPU/设备/宿主机路径的 Compose 复测能力摘要与预检。
2. 复测 MAC 绑定，并检查不同 Compose 版本的 `pull_policy`、GPU 预留和规范化 JSON 差异。
3. 根据结果修正兼容性问题，再加入 zstd/OCI、许可证签发和更细的 MAC 策略。

## 重要命令

```bash
GOCACHE=/tmp/deploybundle-gocache go test -count=1 -race ./...
GOCACHE=/tmp/deploybundle-gocache go vet ./...
for arch in amd64 arm64; do
  GOOS=linux GOARCH=$arch CGO_ENABLED=0 GOCACHE=/tmp/deploybundle-gocache \
    go build -trimpath -buildvcs=false -ldflags='-s -w' \
    -o bin/deploybundle-pack-linux-$arch ./cmd/deploy-pack
  GOOS=linux GOARCH=$arch CGO_ENABLED=0 GOCACHE=/tmp/deploybundle-gocache \
    go build -trimpath -buildvcs=false -ldflags='-s -w' \
    -o bin/deploybundle-run-linux-$arch ./cmd/deploy-run
done
shasum -a 256 bin/deploybundle-*-linux-* > bin/SHA256SUMS
```

## 注意事项

不要把 key、签名私钥、license 或部署主机认证信息写入交接文档或 bundle。宿主 root/Docker 管理员可以读取运行时内容，不能把该工具描述为绝对防提取方案。
