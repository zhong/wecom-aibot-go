package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// DefaultMaxToolRounds 是 RunTurn 允许的最大工具执行轮数（<=0 时的兜底值）。
const DefaultMaxToolRounds = 5

// ErrMaxToolRounds 表示工具执行轮数耗尽仍未得到模型的最终文本回复。
var ErrMaxToolRounds = errors.New("工具执行轮数已达上限，未得到最终回复")

// ToolExecutor 执行一次工具调用并返回给模型的文本结果（通常是 JSON 字符串，
// 如 `{"ok":true}`）。上下文（目标会话等）由闭包注入，模型只能选择工具名
// 与参数，无法伪造投递目标。
type ToolExecutor func(ctx context.Context, name string, args json.RawMessage) (string, error)

// TurnOptions 是 RunTurn / AgentReply 的工具循环配置，零值可用默认。
type TurnOptions struct {
	// MaxToolRounds 限制模型 → 工具 → 模型循环的最大轮数，<=0 用 DefaultMaxToolRounds。
	MaxToolRounds int
	// RoundNotice 非空时在每轮工具执行完后作为文本增量回传一次
	// （如 "正在操作卡片…"），并计入最终文本。
	RoundNotice string
}

// toolUnavailable 是 exec 为空时的占位执行器：把"工具不可用"喂回模型。
func toolUnavailable(context.Context, string, json.RawMessage) (string, error) {
	return "工具不可用", nil
}

// RunTurn 执行多轮「模型 → 工具 → 模型」循环：模型流式产出文本
// （经 onDelta 实时回调，企微桥用它推流式帧）；若模型发起工具调用，
// 逐个执行、把结果作为 role "tool" 消息回喂，继续下一轮，直到模型
// 不再调用工具。返回全程累积的文本。
//
//   - 工具执行错误不中止循环：错误转成文本结果喂回模型，让它自行调整。
//   - ctx 取消/LLM 错误立即返回，已累积的文本随返回值带回，由调用方闭环。
//   - 轮数耗尽返回 (累积文本, ErrMaxToolRounds)。
func RunTurn(ctx context.Context, client Client, chatReq ChatRequest, exec ToolExecutor,
	onDelta func(delta string) error, opts TurnOptions) (finalText string, err error) {

	maxRounds := opts.MaxToolRounds
	if maxRounds <= 0 {
		maxRounds = DefaultMaxToolRounds
	}
	if exec == nil {
		exec = toolUnavailable
	}

	msgs := append([]ChatMessage(nil), chatReq.Messages...)
	var full strings.Builder

	for round := 0; round < maxRounds; round++ {
		r := chatReq
		r.Messages = msgs

		var roundText strings.Builder
		var toolCalls []ToolCall
		_, streamErr := client.StreamChatEvents(ctx, r, func(ev StreamEvent) error {
			switch ev.Type {
			case StreamEventTextDelta:
				if ev.TextDelta == "" {
					return nil
				}
				roundText.WriteString(ev.TextDelta)
				full.WriteString(ev.TextDelta)
				return onDelta(ev.TextDelta)
			case StreamEventToolCall:
				toolCalls = append(toolCalls, ev.ToolCall)
			}
			return nil
		})
		if streamErr != nil {
			return full.String(), streamErr
		}

		if len(toolCalls) == 0 {
			// 模型不再调用工具：本轮文本即最终回复。
			return full.String(), nil
		}
		if round == maxRounds-1 {
			// 下一轮必超限：直接以耗尽错误收场，不再发起注定浪费的一次流。
			break
		}

		// 记录模型的工具调用（可与正文并存），再顺序执行并回喂结果。
		msgs = append(msgs, ChatMessage{Role: "assistant", Content: roundText.String(), ToolCalls: toolCalls})
		for _, tc := range toolCalls {
			result, execErr := exec(ctx, tc.Name, tc.Arguments)
			if execErr != nil {
				if ctx.Err() != nil {
					return full.String(), ctx.Err()
				}
				// 执行失败也要闭环：错误作为结果喂回模型，让它自行调整。
				result = "工具执行失败: " + execErr.Error()
			}
			msgs = append(msgs, ChatMessage{Role: "tool", ToolCallID: tc.ID, Content: result})
		}
		if opts.RoundNotice != "" {
			full.WriteString(opts.RoundNotice)
			if err := onDelta(opts.RoundNotice); err != nil {
				return full.String(), err
			}
		}
	}
	return full.String(), ErrMaxToolRounds
}
