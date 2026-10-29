#!/usr/bin/env bash
#
# DeployBundle 部署脚本
#
# 用途：
#   先验证签名、哈希、加密流和主机预检，再应用 Bundle。
#   这个脚本不会自动执行 docker compose down，不会删除旧容器、网络或卷。
#
# 使用：
#   1. 修改下面“需要修改的配置”中的 Bundle 和密钥路径。
#   2. 确认目标主机 Docker daemon、GPU 驱动和宿主机路径满足 Bundle 预检。
#   3. 执行：./scripts/deploybundle-apply.sh
#
# 回退：
#   指向旧 Bundle 后重新执行本脚本即可。apply 失败时不会自动回滚已加载镜像
#   或持久化数据，需要由操作者根据实际状态决定后续处理。

set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"

# ======================== 需要修改的配置 ========================

# 要验证并部署的 Bundle 目录。
BUNDLE_DIR="release/piloteye-v1-cpu"

# Bundle 加密密钥和验签公钥。
KEY_FILE="keys/key.bin"
VERIFY_KEY="keys/signing.pub"

# 加密 Bundle 中相对 Compose 资产的持久化发布目录。
ASSETS_DIR="/var/lib/deploybundle/assets"

# 留空表示启动 Compose 中全部服务；也可以填写一个或多个服务名。
# 例如：SERVICES=("piloteye-api" "piloteye-web")
SERVICES=()

# 默认执行平台和宿主机资源预检。只有确认必要时才改为 true。
ALLOW_PLATFORM_MISMATCH="false"
SKIP_HOST_PREFLIGHT="false"

# ======================== 以下通常不需要修改 ========================

die() { echo "错误：$*" >&2; exit 1; }

absolute_path() {
  case "$1" in
    /*) printf '%s\n' "$1" ;;
    *) printf '%s/%s\n' "$PROJECT_ROOT" "$1" ;;
  esac
}

require_file() {
  [[ -f "$1" ]] || die "文件不存在：$1"
}

select_run_binary() {
  case "$(uname -m)" in
    x86_64|amd64) echo "$PROJECT_ROOT/bin/deploybundle-run-linux-amd64" ;;
    aarch64|arm64) echo "$PROJECT_ROOT/bin/deploybundle-run-linux-arm64" ;;
    *) die "不支持的本机架构：$(uname -m)" ;;
  esac
}

BUNDLE_PATH="$(absolute_path "$BUNDLE_DIR")"
KEY_PATH="$(absolute_path "$KEY_FILE")"
VERIFY_PATH="$(absolute_path "$VERIFY_KEY")"
RUN_BIN="$(select_run_binary)"

require_file "$RUN_BIN"
[[ -d "$BUNDLE_PATH" ]] || die "Bundle 目录不存在：$BUNDLE_PATH"
require_file "$KEY_PATH"
require_file "$VERIFY_PATH"

COMMON_ARGS=(
  --bundle "$BUNDLE_PATH"
  --key-file "$KEY_PATH"
  --verify-key "$VERIFY_PATH"
)

OPTIONAL_ARGS=()
[[ "$ALLOW_PLATFORM_MISMATCH" == "true" ]] && OPTIONAL_ARGS+=(--allow-platform-mismatch)
[[ "$SKIP_HOST_PREFLIGHT" == "true" ]] && OPTIONAL_ARGS+=(--skip-host-preflight)

echo "使用部署工具：$RUN_BIN"
echo "目标 Bundle：$BUNDLE_PATH"
echo "开始验证 Bundle……"
"$RUN_BIN" verify "${COMMON_ARGS[@]}" "${OPTIONAL_ARGS[@]}"

APPLY_ARGS=(apply "${COMMON_ARGS[@]}" --assets-dir "$ASSETS_DIR" "${OPTIONAL_ARGS[@]}")
for service in "${SERVICES[@]}"; do
  APPLY_ARGS+=(--service "$service")
done

echo "验证通过，开始应用 Bundle……"
"$RUN_BIN" "${APPLY_ARGS[@]}"
echo "Bundle 应用完成：$BUNDLE_PATH"
