#!/bin/sh
# wecom-bot 安装脚本：从 GitHub Releases 下载最新（或指定）版本并安装。
# 用法：
#   curl -fsSL https://raw.githubusercontent.com/lznauy/wecom-bot/main/install.sh | sh
#   或下载后执行：./install.sh [版本号，如 v0.1.0]
set -e

REPO="lznauy/wecom-bot"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

# ---- 识别平台 ----
os="$(uname -s)"
arch="$(uname -m)"
case "$os" in
    Linux) os_name="linux" ;;
    Darwin) os_name="darwin" ;;
    *) echo "错误：不支持的操作系统: $os" >&2; exit 1 ;;
esac
case "$arch" in
    x86_64|amd64) arch_name="amd64" ;;
    aarch64|arm64) arch_name="arm64" ;;
    *) echo "错误：不支持的架构: $arch" >&2; exit 1 ;;
esac

# ---- 确定版本 ----
if [ -n "$1" ]; then
    version="$1"
else
    echo "查询最新版本..."
    version="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')"
    [ -n "$version" ] || { echo "错误：无法获取最新版本号" >&2; exit 1; }
fi
echo "安装 wecom-bot $version ($os_name-$arch_name)"

# ---- 下载并校验 ----
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

asset="wecom-bot-${os_name}-${arch_name}"
binary_url="https://github.com/${REPO}/releases/download/${version}/${asset}"
echo "下载 $binary_url"
curl -fsSL "$binary_url" -o "$tmpdir/wecom-bot"

checksum_url="https://github.com/${REPO}/releases/download/${version}/SHA256SUMS"
echo "校验 SHA256..."
if curl -fsSL "$checksum_url" -o "$tmpdir/SHA256SUMS" 2>/dev/null; then
    expected="$(grep "${asset}\$" "$tmpdir/SHA256SUMS" | awk '{print $1}')"
    if [ -n "$expected" ]; then
        if command -v sha256sum >/dev/null 2>&1; then
            actual="$(sha256sum "$tmpdir/wecom-bot" | awk '{print $1}')"
        else
            actual="$(shasum -a 256 "$tmpdir/wecom-bot" | awk '{print $1}')"
        fi
        [ "$actual" = "$expected" ] || { echo "错误：校验和不匹配 (期望 $expected，实际 $actual)" >&2; exit 1; }
        echo "校验通过"
    else
        echo "警告：SHA256SUMS 中未找到对应条目，跳过校验"
    fi
else
    echo "警告：无法下载 SHA256SUMS，跳过校验"
fi

# ---- 安装 ----
chmod +x "$tmpdir/wecom-bot"
if [ -w "$INSTALL_DIR" ] || mkdir -p "$INSTALL_DIR" 2>/dev/null && [ -w "$INSTALL_DIR" ]; then
    mv "$tmpdir/wecom-bot" "$INSTALL_DIR/wecom-bot"
else
    echo "需要权限写入 $INSTALL_DIR，使用 sudo..."
    sudo mv "$tmpdir/wecom-bot" "$INSTALL_DIR/wecom-bot"
fi

echo
echo "已安装: $INSTALL_DIR/wecom-bot"
"$INSTALL_DIR/wecom-bot" -h 2>&1 | head -1 || true
echo
echo "使用前请设置企业微信机器人凭证："
echo "  export WECOM_BOT_ID=xxx"
echo "  export WECOM_BOT_SECRET=xxx"
echo "  wecom-bot"
