package llm

import (
	"context"
	"fmt"

	wecomaibot "github.com/zhong/wecom-aibot-go"
)

// DefaultErrorText 是 LLM 调用失败且没有部分内容可展示时的兜底文案。
const DefaultErrorText = "抱歉，大模型服务暂时不可用，请稍后重试。"

// DefaultEmptyText 是模型正常结束但没有返回任何内容时的兜底文案。
const DefaultEmptyText = "（模型没有返回内容）"

// streamBridge 封装「占位帧 → 累积帧(NonBlocking) → 终帧(失败降级)」的
// 企微流式帧序列，StreamReply 与 AgentReply 共用：
// 企微 stream 帧按 (req_id, stream.id) 整体替换渲染，跨工具轮保持同一
// streamID 时，后续帧替换为不断增长的累积全文即可。
type streamBridge struct {
	bot      *wecomaibot.Client
	req      wecomaibot.RequestCarrier
	streamID string
	content  string
}

// begin 发一帧 finish=false 的占位帧（阻塞发，让用户立刻看到响应）。
func (b *streamBridge) begin(placeholder string) error {
	if placeholder == "" {
		return nil
	}
	_, err := b.bot.ReplyStreamByID(b.req, b.streamID, placeholder, false, nil, nil)
	return err
}

// push 累积一个文本增量并以非阻塞方式发累积全文帧；
// skipped（ACK 在途时的跳帧）不算失败，天然形成背压。
func (b *streamBridge) push(delta string) error {
	b.content += delta
	_, _, err := b.bot.ReplyStreamNonBlocking(b.req, b.streamID, b.content, false, nil, nil)
	return err
}

// finish 发 finish=true 终帧闭环；终帧本身失败时若 ChatID 可得则降级
// SendMessage 提示（对齐 examples/production 的降级模式）。
func (b *streamBridge) finish(finalText string) error {
	b.content = finalText
	if _, err := b.bot.ReplyStreamByID(b.req, b.streamID, finalText, true, nil, nil); err != nil {
		if chatID := extractChatID(b.req); chatID != "" {
			_, _ = b.bot.SendMessage(chatID, map[string]any{
				"msgtype":  "markdown",
				"markdown": map[string]any{"content": DefaultErrorText},
			})
		}
		return err
	}
	return nil
}

// StreamReply 把一次 LLM 流式对话桥接到企微流式回复（打字机效果），返回
// 终帧实际发出的完整文本（多轮对话场景用它补记 assistant 历史）。
//
// 帧序列与闭环保证：
//  1. placeholder 非空时先发一帧 finish=false 占位
//  2. LLM 每产出一个增量就累积全文，用 ReplyStreamNonBlocking 发累积内容
//     （上一帧 ACK 未回时 NonBlocking 自动跳帧，LLM 流不会被 ACK 等待拖死）
//  3. 正常结束发 finish=true 终帧；模型空回复用 DefaultEmptyText 兜底，流保证闭环
//  4. LLM 中途出错或 ctx 取消：已累积内容非空则带现有内容发终帧，否则发
//     DefaultErrorText 终帧，随后返回原错误
//  5. 终帧本身发送失败时，若 ChatID 可得则降级 SendMessage 提示，并返回终帧错误
func StreamReply(ctx context.Context, bot *wecomaibot.Client, req wecomaibot.RequestCarrier,
	streamID string, client Client, chatReq ChatRequest, placeholder string) (finalText string, err error) {

	b := &streamBridge{bot: bot, req: req, streamID: streamID}
	if err := b.begin(placeholder); err != nil {
		return "", err
	}

	_, streamErr := client.StreamChat(ctx, chatReq, func(delta string) error {
		return b.push(delta)
	})

	finalText = b.content
	switch {
	case streamErr != nil && finalText == "":
		finalText = DefaultErrorText
	case streamErr == nil && finalText == "":
		finalText = DefaultEmptyText
	}

	if err := b.finish(finalText); err != nil {
		if streamErr != nil {
			return finalText, fmt.Errorf("LLM 流式回复失败（终帧也发送失败）: %w", streamErr)
		}
		return finalText, fmt.Errorf("发送流式终帧失败: %w", err)
	}
	// 终帧发出即闭环；LLM 本身的错误（即便已带部分内容发出）仍然上抛给调用方记录。
	return finalText, streamErr
}

// AgentReply 把带工具调用的多轮 Agent 循环（RunTurn）桥接到企微流式回复：
// 帧序列闭环、错误/空回复兜底与降级语义与 StreamReply 完全一致；工具执行
// （如发卡片）穿插在流式帧之间，文本增量照常打字机推送。返回终帧实际发出的
// 完整文本，卡片操作等结构化输出不包含其中（它们作为独立卡片消息送达）。
//
// update_template_card 类「更新已有卡片」的能力不在工具目录内：卡片事件后
// 只有 5 秒更新窗口，一次 LLM 往返不可达；应通过 OnTemplateCardEvent
// 处理器确定性更新（见 examples/llm-bot）。
func AgentReply(ctx context.Context, bot *wecomaibot.Client, req wecomaibot.RequestCarrier,
	streamID string, client Client, chatReq ChatRequest, exec ToolExecutor,
	placeholder string, turnOpts TurnOptions) (finalText string, err error) {

	b := &streamBridge{bot: bot, req: req, streamID: streamID}
	if err := b.begin(placeholder); err != nil {
		return "", err
	}

	_, turnErr := RunTurn(ctx, client, chatReq, exec, func(delta string) error {
		return b.push(delta)
	}, turnOpts)

	finalText = b.content
	switch {
	case turnErr != nil && finalText == "":
		finalText = DefaultErrorText
	case turnErr == nil && finalText == "":
		finalText = DefaultEmptyText
	}

	if err := b.finish(finalText); err != nil {
		if turnErr != nil {
			return finalText, fmt.Errorf("LLM Agent 回复失败（终帧也发送失败）: %w", turnErr)
		}
		return finalText, fmt.Errorf("发送流式终帧失败: %w", err)
	}
	return finalText, turnErr
}

// extractChatID 从消息载体提取 ChatID（用于终帧失败的降级发送）；
// 单聊消息可能没有 ChatID，返回空串表示无法降级。
func extractChatID(req wecomaibot.RequestCarrier) string {
	switch m := req.(type) {
	case wecomaibot.TextMessage:
		return m.ChatID
	case wecomaibot.ImageMessage:
		return m.ChatID
	case wecomaibot.VoiceMessage:
		return m.ChatID
	case wecomaibot.VideoMessage:
		return m.ChatID
	case wecomaibot.FileMessage:
		return m.ChatID
	case wecomaibot.MixedMessage:
		return m.ChatID
	case wecomaibot.EventMessage:
		return m.ChatID
	}
	return ""
}
