# wecom-aibot-go

[![CI](https://github.com/Area1m/wecom-aibot-go/actions/workflows/ci.yml/badge.svg)](https://github.com/Area1m/wecom-aibot-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/Area1m/wecom-aibot-go.svg)](https://pkg.go.dev/github.com/Area1m/wecom-aibot-go)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

企业微信智能机器人 Go SDK（Go 1.22+），基于 WebSocket 长连接通道，提供消息接收、流式回复、模板卡片、主动发送、文件下载与 AES 解密能力。

> **Fork of [Area1m/wecom-aibot-go](https://github.com/Area1m/wecom-aibot-go)**。本 fork 定期同步上游更新；模块引入路径为 `github.com/zhong/wecom-aibot-go`。

- 默认连接地址：`wss://openws.work.weixin.qq.com`
- 并发模型：`goroutine + channel`
- WebSocket 实现：`github.com/gorilla/websocket`
- 日志：可插拔 `Logger` 接口

## 功能特性

- WebSocket 长连接与自动认证（`bot_id + secret`）
- 心跳保活（`ping`）与断线自动重连
- 指数退避重连（最大 30s）
- 消息/事件自动分发（文本、图片、语音、视频、文件、混合消息、事件）
- 同 `req_id` 串行回复队列与 ACK 等待
- 流式回复与流式+卡片组合回复
- 模板卡片回复、欢迎语回复、卡片更新
- 模板卡片交互事件（嵌套 `template_card_event` 解析）
- 反馈事件与连接断开事件（`disconnected_event`）
- 主动发送 Markdown / 模板卡片
- 文件下载与 AES-256-CBC 解密
- 消息引用（`Quote`）与 `response_url` 字段

## 安装

```bash
go get github.com/zhong/wecom-aibot-go
```

> 如果你在本地仓库运行示例，可直接在 `wecom-aibot-go` 目录执行 `go mod tidy && go run ./examples`。

## 快速开始

```go
package main

import (
  "context"
  "log"

  wecomaibot "github.com/zhong/wecom-aibot-go"
)

func main() {
  bot, err := wecomaibot.NewClient(wecomaibot.Config{
    BotID:  "your-bot-id",
    Secret: "your-bot-secret",
  })
  if err != nil {
    log.Fatalf("创建客户端失败: %v", err)
  }

  bot.OnAuthenticated(func(ctx context.Context) {
    log.Println("认证成功")
  })

  bot.OnText(func(ctx context.Context, msg wecomaibot.TextMessage) {
    _, err := bot.ReplyStreamByID(msg, wecomaibot.GenerateReqID("stream"), "你好，我是 Go SDK 机器人", true, nil, nil)
    if err != nil {
      log.Printf("回复失败: %v", err)
    }
  })

  if err := bot.Connect(); err != nil {
    log.Fatalf("连接失败: %v", err)
  }

  select {}
}
```

示例运行：

- 基础示例：`go run ./examples`
- 生产模板：`go run ./examples/production`
- LLM 机器人：`go run ./examples/llm-bot`

## 配置项

`Config` 字段：

- `BotID`：机器人 ID（必填）
- `Secret`：机器人 Secret（必填）
- `ReconnectIntervalMS`：重连基础间隔，默认 `1000`
- `MaxReconnectAttempts`：最大重连次数（连接断开），默认 `10`；`-1` 表示无限；`0` 同样取默认 `10`
- `MaxAuthFailureAttempts`：认证失败最大重试次数，默认 `5`；`-1` 表示无限；`0` 同样取默认 `5`；与重连预算独立
- `HeartbeatIntervalMS`：心跳间隔，默认 `30000`
- `RequestTimeoutMS`：HTTP 下载超时，默认 `10000`
- `TCPUserTimeoutMS`：半开连接检测时限（毫秒），默认 `60000`，仅 Linux 生效
- `AuthTimeoutMS`：等待订阅 ACK 的读超时（毫秒），默认 `15000`，超时未收到订阅 ACK 即重连
- `MaxDownloadBytes`：单次文件下载上限（字节），默认 `104857600`（100 MiB）
- `WSURL`：WebSocket 地址，默认 `wss://openws.work.weixin.qq.com`
- `Logger`：自定义日志器，默认 `DefaultLogger`

> 心跳说明：服务端收到客户端 `ping` 帧后会回 `errcode:0` 的 ACK（req_id 透传）。SDK 据此判定
> 死连接：连续 2 次未收到心跳 ACK 即主动断开并重连（对齐官方 Node SDK）。此外连接失效还能由
> 发送失败（写不出去）或服务端被动断开来发现；半开连接由 TCP_USER_TIMEOUT 兜底。

## 事件注册

连接生命周期：

- `OnConnected`
- `OnAuthenticated`
- `OnDisconnected`
- `OnReconnecting`
- `OnError`

消息与事件：

- `OnMessage`
- `OnText`
- `OnImage`
- `OnMixed`
- `OnVoice`
- `OnVideo`
- `OnFile`
- `OnEvent`
- `OnEnterChat`
- `OnTemplateCardEvent`
- `OnFeedbackEvent`
- `OnDisconnectedEvent`

## 回复与发送 API

- `Reply(req, body, cmd)`：通用回复
- `ReplyStreamByID(req, streamID, content, finish, msgItem, feedback)`：完整流式控制
- `ReplyStreamNonBlocking(req, streamID, content, finish, msgItem, feedback)`：非阻塞流式回复（不会在 reply 队列积压时阻塞等待）
- `ReplyWelcome(req, body)`：欢迎语回复（`body` 为文本或模板卡片消息体）
- `ReplyTemplateCard(req, card, feedback)`
- `ReplyStreamWithCard(req, streamID, content, finish, opts)`
- `UpdateTemplateCard(req, card, userIDs)`
- `SendMessage(chatID, body)`：主动发送（`body` 为 `markdown` 或 `template_card` 消息体）

> **注意**：卡片更新必须在收到交互事件后 **5 秒内** 调用，否则会返回 `errcode=846604`（websocket request expired）。

模板卡片类型常量（对应 `template_card.card_type`）：`TemplateCardTypeTextNotice` / `TemplateCardTypeNewsNotice` / `TemplateCardTypeButtonInteraction` / `TemplateCardTypeVoteInteraction` / `TemplateCardTypeMultipleInteraction`。

## 模板卡片交互

当用户点击交互式模板卡片（如 `button_interaction`、`vote_interaction`、`multiple_interaction`）时，企业微信通过 `aibot_event_callback` 推送 `template_card_event` 事件。事件数据嵌套在 `event.template_card_event` 对象中：

```json
{
  "eventtype": "template_card_event",
  "template_card_event": {
    "card_type": "button_interaction",
    "event_key": "btn_confirm",
    "task_id": "task_001"
  }
}
```

SDK 解析嵌套结构，通过 `EventMessage.Event.TemplateCardEvent` 访问：

```go
bot.OnTemplateCardEvent(func(ctx context.Context, msg wecomaibot.EventMessage) {
    eventKey := msg.Event.TemplateCardEvent.EventKey
    taskID := msg.Event.TemplateCardEvent.TaskID
    log.Printf("卡片交互: event_key=%s task_id=%s", eventKey, taskID)

    // 5秒内更新卡片
    card := wecomaibot.TemplateCard{
        "card_type": "button_interaction",
        "source":    map[string]string{"desc": "my-bot"},
        "main_title": map[string]string{"title": "✅ 已处理"},
        "task_id":   taskID,
        "button_list": []map[string]any{
            {"text": "已完成", "style": 3, "key": "btn_done"},
        },
    }
    _, _ = bot.UpdateTemplateCard(msg, card, nil)
})
```

> **提示**：模板卡片必须包含 `callback_id` 字段，交互回调才会携带 `event_key` / `task_id`。

## 对接大模型（LLM）

`llm` 子包提供与厂商无关的流式对话抽象，把大模型的增量输出桥接成企微流式回复（打字机效果）：

- `llm.Client` 接口：`StreamChat(ctx, req, onDelta)`，每个内容增量回调一次
- 两种内置实现（仅依赖标准库 `net/http`，无第三方 SDK）：
  - `llm.NewOpenAIClient(OpenAIOptions{...})` — OpenAI 兼容 chat-completions 端点，
    覆盖 OpenAI / DeepSeek / GLM / Qwen / vLLM / one-api 等网关（`BaseURL` 覆盖端点，
    兼容 DeepSeek-R1 / GLM 的 `reasoning_content` 思维链增量）
  - `llm.NewAnthropicClient(AnthropicOptions{...})` — Anthropic Claude Messages API
- `llm.StreamReply(...)` — 流式桥：占位帧 → 累积帧（`ReplyStreamNonBlocking` 自限速）→
  终帧；LLM 出错带半截内容或兜底文案闭环，终帧失败降级 `SendMessage`

```go
client, _ := llm.NewOpenAIClient(llm.OpenAIOptions{
    APIKey: "sk-...", BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat",
})
bot.OnText(func(ctx context.Context, msg wecomaibot.TextMessage) {
    final, err := llm.StreamReply(ctx, bot, msg, wecomaibot.GenerateReqID("stream"), client,
        llm.ChatRequest{
            Messages:     []llm.ChatMessage{{Role: "user", Content: msg.Text.Content}},
            SystemPrompt: "你是一个乐于助人的企业微信智能助手",
        }, "正在思考...")
    _ = final // 多轮对话场景用 final 补记 assistant 历史
})
```

完整的多轮对话机器人示例（含每会话有界历史、语音转文本接入、健康检查、优雅停机）见
`examples/llm-bot`：

```bash
export WECOM_BOT_ID=xxx WECOM_BOT_SECRET=xxx
export LLM_PROVIDER=openai        # openai | anthropic
export LLM_API_KEY=sk-... LLM_MODEL=gpt-4o
go run ./examples/llm-bot
```

> 设计说明：库保持无状态——多轮对话历史由示例层维护（per-chat 有界 slice），
> 这样接入方可以自由替换为 Redis / 数据库等持久化实现。

### 工具调用：让模型自主使用企微丰富交互

`llm` 包支持工业界标准的工具调用（Tool Calling）：把企微交互能力注册成工具，
连同 JSON Schema 发给模型，由模型按内容形态自主决定何时发模板卡片、何时发
markdown 补充消息、何时直接文本回复、何时先读用户附件再作答——不需要用户
明确要求；SDK 侧执行工具后把结果回喂模型，循环直到模型给出最终回复。

- `ChatRequest.Tools` / `llm.ToolDef` / `llm.StreamChatEvents`（`StreamChat`
  保持兼容，只回调文本增量）
- `llm.RunTurn(...)` — 多轮「模型 → 工具 → 模型」循环（默认最多 5 轮，
  工具执行失败会作为结果回喂模型自行调整；`TurnOptions.FinalRoundOnly`
  开启终帧净化——成功时终帧只携带最后一轮正文，中间帧保持全程累积）
- `llm.AgentReply(...)` — 带工具循环的流式桥，帧闭环与 `StreamReply` 一致
- `llm.WeComToolDefs()` / `llm.NewWeComExecutor(bot, msg)` — 内置工具目录与
  执行器；目标会话由执行器闭包绑定当前消息，模型无法伪造投递目标：
  - `send_template_card` — 发模板卡片（通知/数据展示/用户交互类内容）
  - `send_markdown` — 补充发送格式化长文本（表格、多层列表等）
  - `read_attachment` — 读取用户消息携带的附件（自动下载并 AES 解密，
    文本类附件回喂内容预览，图片/音视频回喂元信息；用户「引用附件
    @机器人」送达的图片/文件/图文混排同样当作本条消息的附件可读——
    群聊里独立附件不产生回调，引用是官方可达路径）。外部 agent 模式下
    参数 `save: true` 可把二进制附件落盘到 agent 会话目录并返回
    `saved` 本地路径，pi 等带视觉能力的 agent 可直接读图片

```go
chatReq := llm.ChatRequest{
    Messages: hist, SystemPrompt: prompt,
    Tools: llm.WeComToolDefs(),
}
final, err := llm.AgentReply(ctx, bot, msg, wecomaibot.GenerateReqID("stream"),
    client, chatReq, llm.NewWeComExecutor(bot, msg), "正在思考...", llm.TurnOptions{
        FinalRoundOnly: true, // 终帧净化：只携带最后一轮正文
    })
```

> 注意：`update_template_card` 与 `reply_welcome` 有意不在工具目录内——
> 两者分别要在卡片事件/enter_chat 事件后 5 秒窗口内完成，一次 LLM 往返
> 不可达；应在对应的 `OnTemplateCardEvent` / `OnEnterChat` 处理器里
> 确定性完成（见 `examples/llm-bot`）。

## 对接外部 Agent（ACP）

`llm` 包是内置轻量模式（LLM 直连在连接层）；当大脑换成外部成熟 agent
（pi、Claude Code 等）时，`acp` 包提供标准会话面：经 ACP
（[Agent Client Protocol](https://agentclientprotocol.com)，JSON-RPC 2.0
over stdio）把企微消息桥接给 agent——每条企微推送变成一次发往持久
ACP 会话的 `session/prompt`，agent 的 `agent_message_chunk` 流实时映射为
企微流式回复帧（打字机），同一企微会话跨消息天然获得对话记忆。

```go
router, err := acp.NewRouter(ctx, bot, acp.AgentConfig{
    Command: "pi-acp", // ACP 适配器（pi 经 pi-acp，Claude Code 经 claude-agent-acp）
})
if err != nil { ... }
acp.RouterBind(bot, router) // 绑定后文本/语音/图片/文件/视频/图文自动路由
bot.Connect()
```

架构分工（连接层只管会话面）：

```
企微 ⇄ 本SDK（acp.Router = ACP client + 会话路由 + 打字机桥）
         ├─ 会话面: ACP session/prompt ⇄ 外部 agent 子进程
         └─ 工具面（可选）: 进程内 mcp.WeComServer ⇄ 每 agent 会话
              （agent 以 MCP 客户端身份接入，见下节）
```

- `acp.NewRouter(...)` — 拉起 ACP 适配器子进程、完成 initialize 握手；
  每个企微会话（群聊 chatid / 单聊 userid）一个持久 agent 会话
- `acp.RouterBind(bot, router)` — 绑定全部消息回调，自动组装用户文本
  （语音用企微转写、附件转发为元信息提示）
- `acp.Router.Handle(...)` — 手动路由单条消息（自定义组装时用）
- 无人值守权限：agent 发来的 `session/request_permission` 默认自动拒绝
  （安全缺省），`AgentConfig.Permission` 可注入自定义策略；
  适配器在会话建立后推送的 startup info 块按协议时机过滤，不会渲染给用户
- 完整示例见 `examples/acp-bot`（`ACP_COMMAND=pi-acp` 即可跑通 pi，
  前置配置见 [pi 文档](https://github.com/badlogic/pi-mono)：
  `~/.pi/agent/settings.json` 设 `quietStartup: true` 与默认模型）

### 工具面（MCP）：让外部 agent 也能用企微丰富交互

`llm` 内置模式的三个工具（`send_template_card` / `send_markdown` /
`read_attachment`）同样可以暴露给 ACP 外部 agent：`mcp` 子包在进程内起一个
仅回环监听的 MCP（Streamable HTTP）服务端，工具目录与执行器直接复用
`llm.WeComToolDefs()` / `llm.NewWeComExecutor`——同一套能力，两种大脑。

```
mcp.WeComServer（127.0.0.1 随机端口, Bearer 提权）
  └─ /mcp/<chatKey>  ← 每个企微会话一个独立端点
       每 agent 会话的 cwd 目录里 .pi/mcp.json 指向该会话专属端点
```

- **每会话独立 URL（隔离）**：`acp.Router.EnableTools(surface, workDir)`
  为每个新 agent 会话生成 `workDir/<chatKey>/.pi/mcp.json`，指向该会话
  专属端点并以它为 session cwd——模型物理上够不到其他会话；工具参数
  无需 `chat_id`，prompt 无需注入上下文头，会话历史保持干净
- **快照生命周期**：工具只在当前消息处理在途时可用（执行器闭包绑定该
  消息）；消息收尾后调用返回 `ok:false` 自纠文案，模型可据此调整
- **凭据**：启动时生成随机 token，各端点常量时间校验，不打印到日志

启用方式（`examples/acp-bot`）：

```bash
ACP_TOOLS=1 ACP_COMMAND=pi-acp go run ./examples/acp-bot
# ACP_TOOL_WORKDIR 可覆盖会话目录基址（默认 ~/.wecom-aibot/acp-work，
# 须跨重启稳定：pi 的 trust 决策按路径留存）
```

**agent 沙箱化部署**：pi 无内置沙箱且以启动账号全权限运行——面向全员开放的
机器人必须把 agent 关进容器/microVM。`ACP_COMMAND` 换成 `docker` /
`container` 包装 argv 即可（`acp.AgentConfig` 透传 stdio，连接层零改动），
Apple container 路线下工具面用 `ACP_TOOL_ADDR` 直接监听容器网关（省掉
宿主转发器）；方案对比、实测记录与运行形态见
`docs/agent-sandbox-research.md` 与 `examples/acp-bot/SANDBOX.md`。

pi 注意事项：

- 启动时自动把 workDir 根目录合并进 `~/.pi/agent/trust.json`（只增不改）——
  项目级 `.pi/mcp.json` 仅在受信目录加载，而 pi 的 RPC 模式（pi-acp 用）
  无法弹窗询问 trust，必须预先登记
- pi 对 MCP 工具调用**默认不弹权限**（不走 `session/request_permission`，
  也不受 `AgentConfig.Permission` / `ACP_ALLOW_TOOL` 覆盖）——工具面本就
  只含企微发送/读取能力，无 fs/bash 面
- 为什么不经 ACP 的 `session/new.mcpServers` 传工具面：pi-acp（v0.0.34）
  收下该参数但不转发（上游已确认的已知限制），且 RPC 模式无对应方法；
  项目级 MCP 配置是 pi 官方支持的现实路径

其他 agent（如 Claude Code）同理可用各自的项目级 MCP 配置接入同一端点：

```bash
claude mcp add --transport http wecom <url> --header "Authorization: Bearer <token>"
```

## 文件下载与解密

```go
bot.OnImage(func(ctx context.Context, msg wecomaibot.ImageMessage) {
  file, err := bot.DownloadFile(msg.Image.URL, msg.Image.AESKey)
  if err != nil {
    return
  }
  _ = file.Buffer
  _ = file.Filename
})
```

说明：

- 当 `aesKey` 为空时，返回原始下载数据
- 有 `aesKey` 时，执行 AES-256-CBC 解密（IV 取 key 前 16 字节）并手动去除 PKCS#7 填充
- 也可直接调用 `DecryptFile(buf, aesKey)` 对已下载的密文独立解密（对应官方 `decrypt_file`）

## 协议命令对照

- 认证：`aibot_subscribe`
- 心跳：`ping`
- 回复：`aibot_respond_msg`
- 欢迎语回复：`aibot_respond_welcome_msg`
- 更新卡片：`aibot_respond_update_msg`
- 主动发送：`aibot_send_msg`
- 消息回调：`aibot_msg_callback`
- 事件回调：`aibot_event_callback`

## 目录结构

```text
wecom-aibot-go/
  doc.go            包文档与用法示例
  client.go         客户端：连接、连接生命周期回调注册
  websocket.go      WebSocket 会话、帧收发与 ACK 关联
  dial.go           WebSocket 拨号（TCP 选项、代理与超时）
  auth.go           认证
  heartbeat.go      心跳保活
  reconnect.go      指数退避重连
  dispatcher.go     消息/事件分发
  reply.go          回复、流式回复与主动发送
  api.go            HTTP 侧能力（文件下载）
  crypto.go         AES-256-CBC 解密
  logger.go         日志接口与默认实现
  types.go          协议类型、常量与配置
  utils.go          req_id 生成
  llm/              大模型对接子包（OpenAI 兼容 + Anthropic + 流式桥 + 工具调用）
  mcp/              进程内 MCP 服务端子包（ACP 模式的企微工具面，Streamable HTTP）
  acp/              外部 agent 桥接子包（ACP client + 会话路由 + 打字机桥）
  examples/main.go
  examples/production/main.go
  examples/llm-bot/main.go
  examples/acp-bot/main.go
  examples/acp-bot/piconfig.go
  examples/acp-bot/SANDBOX.md  沙箱化 pi-agent 部署示例（docker / Apple container）
  scripts/sandbox/     沙箱镜像 Dockerfile 与构建脚本（两种后端共用）
  docs/                调研与集成文档（沙箱方案对比、连接层集成草图）
  .github/workflows/ci.yml
  LICENSE
```

## 开发与测试

```bash
gofmt -l .          # 检查格式
go vet ./...        # 静态检查
go test ./...       # 单元测试（含基于假 WebSocket 服务端的端到端用例）
go test -race ./... # 竞态检测（需要 cgo）
```

CI 在 GitHub Actions 上跑 `gofmt` / `go vet` / `go build` / `go test -race`，覆盖 Go 1.22 及以上。

## 开源协议

[MIT](LICENSE)

## 生产模板说明

`examples/production/main.go` 内置了常见生产能力：

- 生命周期日志（连接、认证、断开、重连、错误）
- 处理回调的 panic 恢复
- 回复失败重试（固定次数 + 退避间隔）
- 回复失败时降级发送提示消息
- 健康检查端点 `GET /healthz`（默认 `:8080`）

## 设计映射（Node -> Go）

- `WSClient` -> `Client`
- `EventEmitter` -> `OnXxx` handler 注册机制
- `WsConnectionManager` -> `wsConnection`
- `MessageHandler` -> `dispatcher`
- `WeComApiClient` -> `APIClient`
