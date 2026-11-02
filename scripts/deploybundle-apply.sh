#!/usr/bin/env bash
#
# DeployBundle 部署脚本
#
# 用途：
#   直接调用 apply，由 apply 验证实际使用的配置、资产和选中的镜像归档。
#   默认不 load、不 pull、不 build；只有 UPDATE_IMAGES=true 时才准备镜像。
#   这个脚本不会自动执行 docker compose down，不会删除旧容器、网络或卷。
#
# 使用：
#   1. 修改下面“需要修改的配置”中的 Bundle 和密钥路径。
#   2. 确认目标主机 Docker daemon、GPU 驱动和宿主机路径满足 Bundle 预检。
#   3. 执行：./scripts/deploybundle-apply.sh
#
# 回退：
#   配置回退时指向旧 Bundle；如需回退镜像，另设 UPDATE_IMAGES=true 并准备旧镜像。
#   apply 失败时不会自动回滚已加载镜像或持久化数据。

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

# true：先 load 包目录中的 images.tar（或旧加密归档）；没有归档时尝试 pull。
# false：默认只使用已有本地镜像；即使存在 images.tar 也不加载。
UPDATE_IMAGES="false"

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
APPLY_ARGS=(apply "${COMMON_ARGS[@]}" --assets-dir "$ASSETS_DIR" "${OPTIONAL_ARGS[@]}")
[[ "$UPDATE_IMAGES" == "true" ]] && APPLY_ARGS+=(--update-images)
for service in "${SERVICES[@]}"; do
  APPLY_ARGS+=(--service "$service")
done

echo "开始验证并应用 Bundle……"
"$RUN_BIN" "${APPLY_ARGS[@]}"
echo "Bundle 应用完成：$BUNDLE_PATH"
