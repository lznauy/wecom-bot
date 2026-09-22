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
# 下载一律走 releases/latest/download/<资产名>（不走 GitHub API，不受匿名限流影响）；
# API 仅用于获取版本号用于显示与 SHA256SUMS 校验，失败则跳过校验。
if [ -n "$1" ]; then
    version="$1"
else
    echo "查询最新版本..."
    version="$(curl -fsSL --max-time 10 "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' || true)"
fi
if [ -n "$version" ]; then
    echo "安装 wecom-bot $version ($os_name-$arch_name)"
    base_url="https://github.com/${REPO}/releases/download/${version}"
else
    echo "无法获取版本号（API 限流或无网络访问 GitHub API），直接下载最新版 ($os_name-$arch_name)"
    base_url="https://github.com/${REPO}/releases/latest/download"
fi

# ---- 下载并校验 ----
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

asset="wecom-bot-${os_name}-${arch_name}"
echo "下载 ${base_url}/${asset}"
curl -fsSL "${base_url}/${asset}" -o "$tmpdir/wecom-bot"

if [ -n "$version" ]; then
    echo "校验 SHA256..."
    if ! curl -fsSL "${base_url}/SHA256SUMS" -o "$tmpdir/SHA256SUMS" 2>/dev/null; then
        echo "警告：无法下载 SHA256SUMS，跳过校验"
    else
        expected="$(grep "${asset}\$" "$tmpdir/SHA256SUMS" | awk '{print $1}')"
        if [ -z "$expected" ]; then
            echo "警告：SHA256SUMS 中未找到对应条目，跳过校验"
        else
            if command -v sha256sum >/dev/null 2>&1; then
                actual="$(sha256sum "$tmpdir/wecom-bot" | awk '{print $1}')"
            else
                actual="$(shasum -a 256 "$tmpdir/wecom-bot" | awk '{print $1}')"
            fi
            [ "$actual" = "$expected" ] || { echo "错误：校验和不匹配 (期望 $expected，实际 $actual)" >&2; exit 1; }
            echo "校验通过"
        fi
    fi
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
