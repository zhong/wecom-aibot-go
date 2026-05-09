# wecom-aibot-go

企业微信智能机器人 Go SDK（Go 1.22+），基于 WebSocket 长连接通道，提供消息接收、流式回复、模板卡片、卡片交互事件、主动发送、文件下载与 AES 解密能力。

> **Fork of [Area1m/wecom-aibot-go](https://github.com/Area1m/wecom-aibot-go)**，增加了模板卡片交互事件解析、`response_url` 字段等企业微信官方协议已支持但原 SDK 缺失的功能。

- 默认连接地址：`wss://openws.work.weixin.qq.com`
- 并发模型：`goroutine + channel`
- WebSocket 实现：`github.com/gorilla/websocket`
- 日志：可插拔 `Logger` 接口

## 功能特性

- WebSocket 长连接与自动认证（`bot_id + secret`）
- 心跳保活与异常连接判定（连续未收到 ACK 自动断开）
- 指数退避重连（最大 30s）
- 消息/事件自动分发（文本、图片、语音、文件、混合消息、事件）
- 同 `req_id` 串行回复队列与 ACK 等待
- 流式回复与流式+卡片组合回复
- 模板卡片回复、欢迎语回复、卡片更新
- **模板卡片交互事件**（`OnTemplateCardEvent`）— 解析嵌套的 `template_card_event` 中的 `event_key` / `task_id`
- **反馈事件**（`OnFeedbackEvent`）
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
  bot := wecomaibot.NewClient(wecomaibot.Config{
    BotID:  "your-bot-id",
    Secret: "your-bot-secret",
  })

  bot.OnAuthenticated(func(ctx context.Context) {
    log.Println("认证成功")
  })

  bot.OnText(func(ctx context.Context, msg wecomaibot.TextMessage) {
    _, err := bot.ReplyStream(msg, "你好，我是 Go SDK 机器人")
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

## 配置项

`Config` 字段：

- `BotID`：机器人 ID（必填）
- `Secret`：机器人 Secret（必填）
- `ReconnectIntervalMS`：重连基础间隔，默认 `1000`
- `MaxReconnectAttempts`：最大重连次数，默认 `10`，`-1` 表示无限
- `HeartbeatIntervalMS`：心跳间隔，默认 `30000`
- `RequestTimeoutMS`：HTTP 下载超时，默认 `10000`
- `WSURL`：WebSocket 地址，默认 `wss://openws.work.weixin.qq.com`
- `Logger`：自定义日志器，默认 `DefaultLogger`

## 事件注册

连接生命周期：

- `OnConnected`
- `OnAuthenticated`
- `OnDisconnected`
- `OnReconnecting`
- `OnError`

消息与事件：

- `OnMessage`：所有消息（`BaseMessage`）
- `OnText`：文本消息
- `OnImage`：图片消息
- `OnMixed`：混合消息
- `OnVoice`：语音消息
- `OnFile`：文件消息
- `OnEvent`：所有事件（`EventMessage`）
- `OnEnterChat`：进入会话事件
- `OnTemplateCardEvent`：模板卡片交互事件
- `OnFeedbackEvent`：反馈事件

## 回复与发送 API

### 被动回复（`aibot_respond_msg`）

用于回复收到的用户消息，仅支持以下消息类型：

- `Reply(req, body, cmd)`：通用回复
- `ReplyStream(req, content)`：快速流式回复（单条结束）
- `ReplyStreamByID(req, streamID, content, finish, msgItem, feedback)`：完整流式控制
- `ReplyTemplateCard(req, card, feedback)`：模板卡片回复
- `ReplyStreamWithCard(req, streamID, content, finish, opts)`：流式+卡片组合回复

> **注意**：`aibot_respond_msg` **不支持** `text` 和 `markdown` 消息类型，仅支持 `stream` 和 `template_card`。如需回复纯文本，请使用 `ReplyStream`。

### 欢迎语回复（`aibot_respond_welcome_msg`）

- `ReplyWelcomeText(req, content)`：文本欢迎语
- `ReplyWelcomeTemplateCard(req, card)`：卡片欢迎语

> 欢迎语回复支持 `text` 和 `template_card` 两种消息类型。

### 卡片更新（`aibot_respond_update_msg`）

- `UpdateTemplateCard(req, card, userIDs)`：更新已有模板卡片

> 卡片更新必须在收到交互事件后 **5 秒内** 调用，否则会返回 `errcode=846604`（websocket request expired）。

### 主动发送（`aibot_send_msg`）

- `SendMarkdown(chatID, content)`：主动发送 Markdown 消息
- `SendTemplateCard(chatID, card)`：主动发送模板卡片

> 主动发送支持 `markdown`、`text`、`template_card` 等消息类型，无需关联用户消息。

### 文件下载

- `DownloadFile(url, aesKey)`：下载文件并可选 AES-256-CBC 解密

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

SDK 已正确解析嵌套结构，通过 `EventMessage.Event.TemplateCardEvent` 访问：

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

## 数据结构

### 消息类型

| 类型 | 结构体 | 说明 |
|------|--------|------|
| 文本 | `TextMessage` | 含 `Text.Content` |
| 图片 | `ImageMessage` | 含 `Image.URL`、`Image.AESKey` |
| 语音 | `VoiceMessage` | 含 `Voice.Content` |
| 文件 | `FileMessage` | 含 `File.URL`、`File.AESKey` |
| 混合 | `MixedMessage` | 含 `Mixed.MsgItem` |
| 事件 | `EventMessage` | 含 `Event.EventType`、`Event.TemplateCardEvent` |

### 公共字段（`BaseMessage`）

- `MsgID`：消息 ID
- `AIBotID`：机器人 ID
- `ChatID`：群聊 ID（群消息时有值）
- `ChatType`：聊天类型（`group` / `single`）
- `From`：发送者信息（`From.UserID`）
- `CreateTime`：消息创建时间
- `MsgType`：消息类型
- `Quote`：引用消息（可选）
- `ResponseURL`：响应 URL（可选）

### 事件结构（`EventMessage`）

除 `BaseMessage` 的公共字段外：

- `Event.EventType`：事件类型（`enter_chat`、`template_card_event`、`feedback_event`）
- `Event.EventKey`：事件键（顶层，部分事件为空）
- `Event.TaskID`：任务 ID（顶层，部分事件为空）
- `Event.TemplateCardEvent`：卡片交互事件数据（嵌套）
  - `CardType`：卡片类型
  - `EventKey`：交互事件键
  - `TaskID`：任务 ID
- `ResponseURL`：响应 URL（可选）

## 协议命令对照

| 命令 | 用途 | 支持的消息类型 |
|------|------|---------------|
| `aibot_subscribe` | 认证 | — |
| `ping` | 心跳 | — |
| `aibot_respond_msg` | 被动回复 | `stream`、`template_card`、`stream_with_template_card` |
| `aibot_respond_welcome_msg` | 欢迎语回复 | `text`、`template_card` |
| `aibot_respond_update_msg` | 更新卡片 | `update_template_card` |
| `aibot_send_msg` | 主动发送 | `markdown`、`text`、`template_card` 等 |
| `aibot_msg_callback` | 消息回调 | — |
| `aibot_event_callback` | 事件回调 | — |

## 目录结构

```text
wecom-aibot-go/
  client.go        — Client 构造与生命周期
  websocket.go     — WebSocket 连接管理与回复队列
  auth.go          — 认证帧发送
  heartbeat.go     — 心跳保活
  reconnect.go     — 指数退避重连
  dispatcher.go    — 消息/事件分发
  message.go       — 消息解码辅助
  reply.go         — 回复与发送 API
  api.go           — HTTP 下载客户端
  crypto.go        — AES-256-CBC 解密
  logger.go        — 日志接口与默认实现
  types.go         — 所有数据类型定义
  utils.go         — 工具函数（ReqID 生成等）
  examples/main.go — 基础示例
  examples/production/main.go — 生产模板
```

## 生产模板说明

`examples/production/main.go` 内置了常见生产能力：

- 生命周期日志（连接、认证、断开、重连、错误）
- 处理回调的 panic 恢复
- 回复失败重试（固定次数 + 退避间隔）
- 回复失败时降级发送提示消息
- 健康检查端点 `GET /healthz`（默认 `:8080`）

## 相对原仓库的变更

- 修正 `EventContent` 结构体，新增 `TemplateCardEvent` 嵌套字段，正确解析卡片交互回调中的 `event_key` / `task_id`
- `EventMessage` 和 `BaseMessage` 新增 `ResponseURL` 字段
- 模块路径变更为 `github.com/zhong/wecom-aibot-go`
- 新增事件类型常量 `EventTypeTemplateCardEvent`、`EventTypeFeedbackEvent`

## 设计映射（Node -> Go）

- `WSClient` → `Client`
- `EventEmitter` → `OnXxx` handler 注册机制
- `WsConnectionManager` → `wsConnection`
- `MessageHandler` → `dispatcher`
- `WeComApiClient` → `APIClient`
