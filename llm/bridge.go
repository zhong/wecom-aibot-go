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

// StreamReply 把一次 LLM 流式对话桥接到企微流式回复（打字机效果），返回
// 终帧实际发出的完整文本（多轮对话场景用它补记 assistant 历史）。
//
// 帧序列与闭环保证：
//  1. placeholder 非空时先发一帧 finish=false 占位（阻塞发，让用户立刻看到响应）
//  2. LLM 每产出一个增量就累积全文，用 ReplyStreamNonBlocking 发累积内容
//     （企微 stream 帧按 (req_id, stream.id) 整体替换渲染；上一帧 ACK 未回时
//     NonBlocking 自动跳帧，天然形成背压，LLM 流不会被 ACK 等待拖死）
//  3. 正常结束发 finish=true 终帧；模型空回复用 DefaultEmptyText 兜底，流保证闭环
//  4. LLM 中途出错或 ctx 取消：已累积内容非空则带现有内容发终帧，否则发
//     DefaultErrorText 终帧，随后返回原错误
//  5. 终帧本身发送失败时，若 ChatID 可得则降级 SendMessage 提示（对齐
//     examples/production 的降级模式），并返回终帧错误
func StreamReply(ctx context.Context, bot *wecomaibot.Client, req wecomaibot.RequestCarrier,
	streamID string, client Client, chatReq ChatRequest, placeholder string) (finalText string, err error) {

	if placeholder != "" {
		if _, err := bot.ReplyStreamByID(req, streamID, placeholder, false, nil, nil); err != nil {
			return "", err
		}
	}

	var content string
	_, streamErr := client.StreamChat(ctx, chatReq, func(delta string) error {
		content += delta
		_, _, err := bot.ReplyStreamNonBlocking(req, streamID, content, false, nil, nil)
		// skipped（ACK 在途时的跳帧）不算失败；只有真实错误中止 LLM 流。
		return err
	})

	finalText = content
	switch {
	case streamErr != nil && content == "":
		finalText = DefaultErrorText
	case streamErr == nil && content == "":
		finalText = DefaultEmptyText
	}

	if _, err := bot.ReplyStreamByID(req, streamID, finalText, true, nil, nil); err != nil {
		if chatID := extractChatID(req); chatID != "" {
			_, _ = bot.SendMessage(chatID, map[string]any{
				"msgtype":  "markdown",
				"markdown": map[string]any{"content": DefaultErrorText},
			})
		}
		if streamErr != nil {
			return finalText, fmt.Errorf("LLM 流式回复失败（终帧也发送失败）: %w", streamErr)
		}
		return finalText, fmt.Errorf("发送流式终帧失败: %w", err)
	}
	// 终帧发出即闭环；LLM 本身的错误（即便已带部分内容发出）仍然上抛给调用方记录。
	return finalText, streamErr
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
