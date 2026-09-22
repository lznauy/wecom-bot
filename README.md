# wecom-bot

企业微信 AI 机器人守护进程。将企业微信群聊 / 私聊消息桥接到 [NekoCode](https://github.com/lznauy/NekoCode) AI 编程助手 —— 在群里 @机器人 发消息，机器人驱动一个 nekocode 子进程处理并回复结果。

```
企业微信 (WebSocket) ⇄ wecom-bot daemon ⇄ 每群一个 nekocode --headless 子进程
```

## 功能特性

- **官方 WebSocket 协议接入**：企业微信"智能机器人"协议（认证订阅、心跳、被动回复、主动发送），断线自动重连（指数退避，1s → 30s 封顶）
- **按会话路由**：群聊按 ChatID、私聊按发送者 UserID 路由到独立 session，会话间上下文隔离
- **正常对话体验**：群聊 @机器人 前缀自动剥离后交给模型；只回复最终结果，不发中间输出
- **消息去重**：msgid 去重（TTL 10 分钟、容量上限 4096），抑制平台重投递；session 内另有 uuid 去重
- **语音支持**：语音消息自动取转写文本
- **工具自动审批**：群聊场景无人值守，nekocode 的工具调用请求一律放行，反向提问一律拒绝
- **会话恢复**：chatID → session_id 映射持久化到工作区（`.wecom-sessions.json`），daemon 重启或子进程重建后自动 `--resume` 恢复对话上下文；恢复失败自动回退新建会话
- **文件发送**：`/file <路径>` 命令将工作区文件上传（分片上传，上限约 50MB）后发送到会话
- **共享工作区（可选）**：通过 `WECOM_SHARED_WORKDIR` 让所有会话共用同一个已存在的文档工作区

## 架构

```
┌─────────────┐   wss    ┌──────────┐  NDJSON/stdio  ┌──────────────────┐
│  企业微信     │ ⇄──────⇄ │ wecom-bot │ ⇄────────────⇄ │ nekocode-tui      │
│  智能机器人   │          │  (Go)     │  （每会话一进程） │ --headless 子进程  │
└─────────────┘          └──────────┘                └──────────────────┘
```

三个模块：

| 模块 | 职责 |
|---|---|
| `wecom/` | 企业微信 WS 协议客户端：认证、30s 心跳（75s 读超时探测）、消息/事件解析、被动回复（stream）与主动发送（Markdown / 文件，带 ack 等待、20KB 文本截断）、临时素材分片上传。同 BotID 被抢占时返回 `ErrSuperseded`，进程退出而非重连 |
| `router/` | chatID → Session 映射；会话按需创建、退出自动清理；msgID 去重；@前缀剥离与 `/file` 命令处理；会话恢复映射的持久化；为每个会话分配工作目录 |
| `session/` | 管理 nekocode `--headless`（可选 `--resume`）子进程，NDJSON over stdio 双向通信；提取 session_id；只把 `result`（最终结果）帧回发到群；自动应答 `control_request` |

### 消息处理流程

1. 收到消息 → msgID 去重 → 确定 chatID（群 ChatID 或私聊 UserID）
2. 群聊消息剥离 `@机器人` 前缀；`/file` 命令转入文件发送流程，不进会话
3. 该 chatID 无会话则创建：按持久化映射尝试 `--resume` 恢复，否则新建；子进程 cwd 设为对应工作目录
4. 消息正文转发给子进程，AI 处理期间群内保持安静（无"处理中"提示）
5. 最终结果（`result` 帧）以 Markdown 回发群聊（30s 超时）；中间输出不发送
6. 发送失败或子进程退出 → 销毁会话，提示重新发送；15s 内即失败的 resume 会话自动清除映射回退新建

## 配置

支持命令行参数或环境变量（`docker-compose` 用环境变量）：

| 参数 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `-bot-id` | `WECOM_BOT_ID` | — | 企业微信机器人 BotID（必填） |
| `-secret` | `WECOM_BOT_SECRET` | — | 机器人 Secret（必填） |
| `-bin` | `NEKOCODE_BIN` | `nekocode-tui` | nekocode CLI 可执行文件路径 |
| `-workdir` | `WECOM_WORKDIR` | 进程当前目录 | 每群独立工作区的父目录，会话目录为 `<workdir>/<chatID>` |
| `-shared-workdir` | `WECOM_SHARED_WORKDIR` | — | 所有群共用的已存在工作区；设置后忽略 `-workdir` 的按群划分 |

## 部署（Docker）

```bash
# .env 文件
WECOM_BOT_ID=xxx
WECOM_BOT_SECRET=xxx

docker compose up -d
```

首次启动时 entrypoint 会从挂载的 NekoCode 源码（`/opt/NekoCode`）自动构建 `nekocode-tui`。容器内保留 Go 工具链正是为此。

docker-compose 挂载：

- `../NekoCode:/opt/NekoCode:ro` — NekoCode 源码，用于构建 CLI
- `./workspace:/app/workspace` — 工作区持久化目录

## 本地运行

```bash
go build -o wecom-bot .
# 不设 -workdir 时，进程当前目录即工作区
./wecom-bot -bot-id xxx -secret xxx -bin /path/to/nekocode-tui
```

## 工作区说明

| 模式 | 配置 | 行为 |
|---|---|---|
| 按群隔离（默认） | `-workdir <父目录>` | 每群一个子目录 `<父目录>/<chatID>/`，群间文件互不影响。chatID 子目录由 daemon 首次建会话时自动创建 |
| 共享工作区 | `-shared-workdir <目录>` | 所有会话直接在该目录启动，共享同一批文档（一个群改动其他群立即可见；对话上下文仍按群隔离） |
| 不设置 | — | 进程当前目录作为工作区（所有群共用） |

### 文件发送命令

在群聊或私聊中发送（群聊可带 @机器人 前缀）：

```
/file                    # 不带参数：回复当前工作目录路径
/file README.md          # 工作区根目录下的文件
/file docs/报告.docx      # 子目录文件
```

- 路径相对于会话工作目录；绝对路径也支持，但必须在工作区内
- 仅允许工作区内的文件，越界路径（含 `../`）会被拒绝
- 命令必须位于消息开头（@ 前缀剥离后），避免普通对话误触发
- 走企业微信分片上传协议（`aibot_upload_media_init/chunk/finish`，单分片 512KB，最多 100 片）

## 日志排查

daemon 输出完整消息生命周期日志（`docker logs` 可见）：

```
[router] message: chat=wrkXXX from=zhang text="帮我看看README"   ← 收到消息
[router] session created: wrkXXX (total 1)                       ← 建会话
[router] delivered result to wrkXXX (1234 bytes)                 ← 回复发成功
```

失败场景：会话创建失败、resume 失败回退、消息转发失败、群投递失败、文件上传/发送失败、连接断开重连、子进程退出，均有对应 `[router]` 日志；nekocode 子进程自身 stderr 直通 daemon 输出。

## 退出行为

- 收到 SIGINT/SIGTERM 后取消 context，活跃 nekocode 子进程被终止，2s 清理等待后退出
- 同一 BotID 被其他客户端连接接管时，进程直接退出（重连无意义）
