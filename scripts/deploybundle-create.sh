#!/usr/bin/env bash
#
# DeployBundle 创建脚本
#
# 用途：
#   使用当前目录中的 Compose 和 Docker 镜像生成签名加密 Bundle，
#   并在创建完成后执行一次 inspect。这个脚本只创建 Bundle，
#   不会停止容器、删除旧 Bundle 或修改远程服务器。
#
# 使用：
#   1. 修改下面“需要修改的配置”中的 Compose、输出目录和 Bundle 标识。
#   2. 确认 Docker daemon 中已经存在 Compose 引用的全部镜像。
#   3. 执行：./scripts/deploybundle-create.sh
#
# 密钥说明：
#   key.bin 是 Bundle 加密密钥，signing.seed 是签名私钥，signing.pub 是验签公钥。
#   三个文件都不会由脚本生成或打印内容；请不要提交到 Git。

set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"

# ======================== 需要修改的配置 ========================

# 输入 Compose 文件。CPU/GPU 打包时分别改为对应文件。
COMPOSE_FILE="deploy-v1/compose.cpu.yml"

# Bundle 输出目录。目录已存在时脚本会拒绝覆盖。
OUTPUT_DIR="release/piloteye-v1-cpu"

# Bundle 加密密钥和签名私钥。路径相对于项目根目录时会自动转换。
KEY_FILE="keys/key.bin"
SIGNING_KEY="keys/signing.seed"

# 创建完成后 inspect 使用的验签公钥。
VERIFY_KEY="keys/signing.pub"

# Bundle 标识必须稳定；生成增量 Bundle 时要和基础 Bundle 保持一致。
BUNDLE_ID="piloteye-v1-cpu"

# 发布版本和运行 profile，写入 Bundle manifest。
RELEASE="2026.10.26"
PROFILE="host-integrated-v1"

# 目标 Docker 平台。CPU/GPU 测试机均为 Linux amd64 时保持此值。
PLATFORM="linux/amd64"

# true：把 Compose 引用的项目内相对配置文件一起加密打包。
PACKAGE_ASSETS="true"

# 可选 Compose 插值环境文件；不使用时留空。最终值会固化进加密 Compose。
ENV_FILE=""

# 可选：填写上一版 Bundle 目录后生成增量 Bundle；不使用时留空生成完整 Bundle。
BASE_BUNDLE=""

# 可选：只保留上一版 manifest.json 时填写此项；与 BASE_BUNDLE 互斥。
BASE_MANIFEST=""

# 可选 Compose project 名；通常留空即可。
PROJECT_NAME=""

# 可选：重复填写要启用的 Compose profile，例如 COMPOSE_PROFILES=("gpu")。
COMPOSE_PROFILES=()

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

select_pack_binary() {
  case "$(uname -m)" in
    x86_64|amd64) echo "$PROJECT_ROOT/bin/deploybundle-pack-linux-amd64" ;;
    aarch64|arm64) echo "$PROJECT_ROOT/bin/deploybundle-pack-linux-arm64" ;;
    *) die "不支持的本机架构：$(uname -m)" ;;
  esac
}

COMPOSE_PATH="$(absolute_path "$COMPOSE_FILE")"
OUTPUT_PATH="$(absolute_path "$OUTPUT_DIR")"
KEY_PATH="$(absolute_path "$KEY_FILE")"
SIGNING_PATH="$(absolute_path "$SIGNING_KEY")"
VERIFY_PATH="$(absolute_path "$VERIFY_KEY")"
ENV_PATH=""
BASE_PATH=""
BASE_MANIFEST_PATH=""
[[ -n "$ENV_FILE" ]] && ENV_PATH="$(absolute_path "$ENV_FILE")"
[[ -n "$BASE_BUNDLE" ]] && BASE_PATH="$(absolute_path "$BASE_BUNDLE")"
[[ -n "$BASE_MANIFEST" ]] && BASE_MANIFEST_PATH="$(absolute_path "$BASE_MANIFEST")"

PACK_BIN="$(select_pack_binary)"
RUN_BIN="${PACK_BIN/pack/run}"

require_file "$PACK_BIN"
require_file "$RUN_BIN"
require_file "$COMPOSE_PATH"
require_file "$KEY_PATH"
require_file "$SIGNING_PATH"
require_file "$VERIFY_PATH"
[[ -z "$ENV_PATH" ]] || require_file "$ENV_PATH"
[[ -z "$BASE_PATH" ]] || [[ -d "$BASE_PATH" ]] || die "基础 Bundle 目录不存在：$BASE_PATH"
[[ -z "$BASE_MANIFEST_PATH" ]] || [[ -f "$BASE_MANIFEST_PATH" ]] || die "基础 manifest 文件不存在：$BASE_MANIFEST_PATH"
[[ -z "$BASE_PATH" || -z "$BASE_MANIFEST_PATH" ]] || die "BASE_BUNDLE 与 BASE_MANIFEST 不能同时填写"
[[ ! -e "$OUTPUT_PATH" ]] || die "输出目录已存在，为避免覆盖而停止：$OUTPUT_PATH"

ARGS=(
  create
  --compose "$COMPOSE_PATH"
  --out "$OUTPUT_PATH"
  --key-file "$KEY_PATH"
  --signing-key "$SIGNING_PATH"
  --bundle-id "$BUNDLE_ID"
  --release "$RELEASE"
  --profile "$PROFILE"
  --platform "$PLATFORM"
)

[[ -z "$ENV_PATH" ]] || ARGS+=(--env-file "$ENV_PATH")
[[ -z "$BASE_PATH" ]] || ARGS+=(--base-bundle "$BASE_PATH")
[[ -z "$BASE_MANIFEST_PATH" ]] || ARGS+=(--base-manifest "$BASE_MANIFEST_PATH")
[[ -z "$PROJECT_NAME" ]] || ARGS+=(--project-name "$PROJECT_NAME")
[[ "$PACKAGE_ASSETS" == "true" ]] && ARGS+=(--package-assets)
for compose_profile in "${COMPOSE_PROFILES[@]}"; do
  ARGS+=(--compose-profile "$compose_profile")
done

echo "使用打包工具：$PACK_BIN"
echo "输入 Compose：$COMPOSE_PATH"
echo "输出 Bundle：$OUTPUT_PATH"
"$PACK_BIN" "${ARGS[@]}"

echo "开始检查 Bundle 清单……"
"$RUN_BIN" inspect --bundle "$OUTPUT_PATH" --verify-key "$VERIFY_PATH"
echo "Bundle 创建并 inspect 完成：$OUTPUT_PATH"
