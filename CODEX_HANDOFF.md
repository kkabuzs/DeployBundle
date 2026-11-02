# Codex 工作交接

更新时间：2026-11-02（Asia/Shanghai）
当前主机：本机 macOS；测试服务器 root@101.200.219.174
项目路径：/Users/dp/project/DeployBundle
当前分支：main；代码与发布物已分两次提交并推送，交接文档按新增授权单独本地提交

## 当前目标

已完成独立加密配置包及显式镜像更新开关，完成本地测试、四个 Linux 二进制构建和 174 隔离验证。正式客户交付和现有服务未更新。

## 已确认事实

- /data/localbyd 是 174 部署与测试内容；/data/byd 是最终客户交付产物，本次只读核对，未修改。
- create 只生成完整加密 Compose 和可选完整资产：full/config-only，schema 1（无资产）或 2（有资产）。无镜像归档/身份/基线 metadata，不 inspect/save/tag/加密镜像。
- 原 image、pull_policy、显式卷名、网络、端口和挂载保持输入语义；环境变量继续经 Compose CLI 解析后固化。
- --base-bundle、--base-manifest、--reuse-images-from 已移除，传入会明确报错。新配置包需要新版 runner。
- 默认 apply 验证配置/资产，执行 up -d --pull never --no-build；不 load/pull/build，忽略普通 images.tar 和旧加密镜像归档。
- --update-images 才按 images.tar > 旧 manifest 声明的加密镜像归档 > Compose pull --policy always 准备镜像。
- load 失败停止，不 up 或回退 pull；pull 失败警告后尝试本地镜像，本地缺失时启动失败。本地启动成功也明确提示镜像更新未确认。
- --service 的 pull 同步选定服务并使用 --include-deps；up 保留原生依赖行为。
- images.tar 不加密、不在配置签名覆盖内；verify 不认证外部普通 tar。
- 独立 verify 完整验证旧包，包括旧加密镜像归档。快捷部署脚本已取消无条件 verify，直接依赖 apply 验证实际使用内容。
- 新配置包资产按 bundle-id 和资产内容确定目录，配置/release 变化而资产不变时路径稳定；旧包沿用原 manifest digest/资产引用路径，签名序列化布局不变。
- 旧 schema 1/2 镜像包及 schema 3 增量包保留读取兼容。旧资产引用仍需要对应旧目录；新包不产生引用。
- 不维护客户当前 release，不比较镜像 ID，不自动 down/prune/删除卷或恢复数据库。
- 同一栈保持 bundle-id/项目名和数据卷映射正确；既有 CPU/GPU 项目名分别是 piloteye-runtest-cpu-0e0e8ca2、piloteye-runtest-gpu-b8861ea0。

## 基于证据的判断

先前四库 recreate 已查明：旧 CPU 增量包删除了 15 个顶层 volumes.*.name，例如 piloteye-runtest_middle_mysql_data 被解析为 piloteye-runtest-cpu-0e0e8ca2_middle_mysql_data，服务 hash 和镜像相同仍会因换卷重建。

前阶段证据在 /data/localbyd/recreate-check-20261102-001。本轮未修正 /data/byd 的旧客户增量，也未修改 deploy-v1 业务 Compose。新版 runner 不会自动修复旧包卷名遗漏；后续输入 Compose 必须保留正确物理卷名。

## 尚未验证的可能性

- 客户现场旧/新卷内容、是否有新增写入尚未核对，不能断言客户数据完整或旧卷存在。
- ARM64 已编译并验证 ELF/SHA，未在 ARM64 Linux 实机执行。
- 首次将旧资产包迁到新格式可能改变资产目录，应先检查挂载；本次未迁移现有业务目录。
- Compose 和 load 不是事务，失败后可能保留部分镜像或容器变化，程序不自动回滚数据。

## 已完成

- 已在当前 main 分两次提交并推送至 origin/main：decb8be（简化配置包并显式控制镜像更新，11 个文件）、12f18f0（更新 Linux 发布二进制和校验清单，5 个文件）。已通过 ls-remote 核对远端 main 为 12f18f08f56b8f69c26998f351771ba77976eb74。上述两次提交未包含交接文档；现按用户新增授权单独提交本交接文档，尚未推送。未新建分支、未操作 174 服务，全部既有未跟踪文件继续排除。
- pack/runner/manifest、相关单元和集成测试、README、两个 Shell 脚本已更新。
- apply 脚本 UPDATE_IMAGES=false 默认，仅为 apply 追加 --update-images。
- 本地 go test -count=1 -race ./...、go vet ./...、bash -n、git diff --check 全部通过。
- 四个静态 Linux ELF 已重建，bin/SHA256SUMS 校验通过。
- 174 隔离目录 /data/localbyd/config-images-check-20261102-001，共 11 项真实检查通过。
- 四库默认更新时容器/卷/测试数据不变，未变资产路径稳定；无效 tar 被默认忽略，pull_policy:always 被启动命令覆盖。
- 真实 save/load、load 失败停止、pull 失败本地回退、缺镜像失败、临时 127.0.0.1 Registry V2 真实 pull 成功均验证通过；没有访问外部镜像仓库。
- PostgreSQL 测试曾遇到初始化临时进程退出窗口，仅修正测试等待最终 PID 1 就绪后继续，产品代码无须修复。
- 已清理 11 个测试容器、11 个测试卷、4 个测试网络、11 个专用镜像标签；临时 HTTP 仓库已关闭。
- 最终 127 个原运行容器的 ID/状态/启动时间/重启计数/镜像/挂载不变，原镜像引用/卷/网络保留，无测试卷和网络残留。
- /data/byd 两个 CPU 包的 manifest/signature/compose 文件 SHA-256、mtime 和大小前后相同。

## 修改文件

cmd/deploy-pack/main.go、main_test.go；
cmd/deploy-run/main.go、main_test.go、assets.go；
pkg/bundle/manifest.go、manifest_test.go；
integration/cli_test.go；
README.md、scripts/deploybundle-create.sh、scripts/deploybundle-apply.sh；
bin 四个 Linux 二进制和 SHA256SUMS；
本交接及 174 /data/localbyd/CODEX_HANDOFF.md。

## 验证结果

当前发布 SHA-256：
8dc14be138216737ae098ac1cd216f0f8430578b12b413f878a7f6cd6dcca798  bin/deploybundle-pack-linux-amd64
8208602c7d3d915f2492c00a8329a7331dc709e5538ce26f865e45de72876c28  bin/deploybundle-pack-linux-arm64
e362ba798f21a4b9553bbbb22e87f60b32fee958d5b029f07b131bf5e401147f  bin/deploybundle-run-linux-amd64
f69d5ce4baa12d024b934aea28705331819492d28b8b4dd84ac4a1722c3e7c8b  bin/deploybundle-run-linux-arm64

174 新 amd64 副本位于：
/data/localbyd/config-images-check-20261102-001/tools/deploybundle-pack-linux-amd64
/data/localbyd/config-images-check-20261102-001/tools/deploybundle-run-linux-amd64

同目录 evidence/results.json 保存 11 项结果，docker-commands.log 保存真实命令，registry-requests.json 保存本机仓库请求，protection-check.json 保存资源保护检查，cleanup-inventory.json 保存清理清单，delivery-before.json/delivery-after.json 保存交付文件指纹。cases/ 保存各项配置、加密测试包和日志。

## 未解决问题

软件开发和隔离验证已完成，无阻塞项。正式交付包替换、客户源 Compose 的卷名修正、客户部署/回滚均未执行。

## 下一步

按用户后续授权处理交付。先核对源 Compose 和客户卷映射，再生成独立配置包；不要直接再次应用存在卷名遗漏的旧 CPU 增量包。

## 重要命令

默认配置更新：deploybundle-run-linux-amd64 apply --bundle ... --key-file ... --verify-key ...
镜像更新：相同命令追加 --update-images
结果：cat /data/localbyd/config-images-check-20261102-001/evidence/results.json
保护检查：cat /data/localbyd/config-images-check-20261102-001/evidence/protection-check.json

## 注意事项

不擅自改 /data/byd 或更新现有服务。不删除客户旧/新卷，不恢复数据库数据。
保留工作区原有未跟踪文件，不 reset/clean/批量覆盖，不擅自提交 deploy-v1、FINAL_REQUIREMENTS.md、semantic-review 或临时文件。
密钥内容须与 Bundle 匹配；交接不记录密码、API Key、Token、密钥内容、完整环境变量或认证信息。
