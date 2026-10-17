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
连同 JSON Schema 发给模型，由模型自行决定何时发模板卡片、何时发 markdown
补充消息；SDK 侧执行工具后把结果回喂模型，循环直到模型给出最终回复。

- `ChatRequest.Tools` / `llm.ToolDef` / `llm.StreamChatEvents`（`StreamChat`
  保持兼容，只回调文本增量）
- `llm.RunTurn(...)` — 多轮「模型 → 工具 → 模型」循环（默认最多 5 轮，
  工具执行失败会作为结果回喂模型自行调整）
- `llm.AgentReply(...)` — 带工具循环的流式桥，帧闭环与 `StreamReply` 一致
- `llm.WeComToolDefs()` / `llm.NewWeComExecutor(bot, msg)` — 内置工具目录
  （`send_template_card` / `send_markdown`）与执行器；目标会话由执行器闭包
  绑定当前消息，模型无法伪造投递目标

```go
chatReq := llm.ChatRequest{
    Messages: hist, SystemPrompt: prompt,
    Tools: llm.WeComToolDefs(),
}
final, err := llm.AgentReply(ctx, bot, msg, wecomaibot.GenerateReqID("stream"),
    client, chatReq, llm.NewWeComExecutor(bot, msg), "正在思考...", llm.TurnOptions{})
```

> 注意：`update_template_card` 有意不在工具目录内——卡片事件后只有 5 秒
> 更新窗口，一次 LLM 往返不可达；卡片更新应在 `OnTemplateCardEvent`
> 处理器里确定性完成（见 `examples/llm-bot`）。

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
  examples/main.go
  examples/production/main.go
  examples/llm-bot/main.go
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
