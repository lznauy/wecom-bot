#!/bin/sh
set -e

# 首次启动：从挂载的 NekoCode 源码构建 nekocode-tui（已存在则跳过）
if ! command -v nekocode-tui >/dev/null 2>&1; then
    if [ -f /opt/NekoCode/go.mod ]; then
        echo "[entrypoint] building nekocode-tui from /opt/NekoCode ..."
        (cd /opt/NekoCode && go build -o /usr/local/bin/nekocode-tui ./cmd/tui)
    else
        echo "[entrypoint] WARNING: nekocode-tui 不可用且未挂载 /opt/NekoCode，会话将无法启动" >&2
    fi
fi

exec wecom-bot \
    ${WECOM_BOT_ID:+-bot-id "$WECOM_BOT_ID"} \
    ${WECOM_BOT_SECRET:+-secret "$WECOM_BOT_SECRET"} \
    ${NEKOCODE_BIN:+-bin "$NEKOCODE_BIN"} \
    ${WECOM_WORKDIR:+-workdir "$WECOM_WORKDIR"} \
    ${WECOM_SHARED_WORKDIR:+-shared-workdir "$WECOM_SHARED_WORKDIR"}
