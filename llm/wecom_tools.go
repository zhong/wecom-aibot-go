package llm

import (
	"context"
	"encoding/json"
	"fmt"

	wecomaibot "github.com/zhong/wecom-aibot-go"
)

// WeComToolDefs 返回企微交互能力的工具目录，连同 JSON Schema 一起发给
// 模型，由模型自行决定何时调用。目标会话由 NewWeComExecutor 的闭包固定，
// 模型只能选择工具与内容。
//
// 注意 update_template_card 有意不在目录内：template_card_event 后只有
// 5 秒更新窗口，一次 LLM 往返不可达；卡片更新应走确定性的
// OnTemplateCardEvent 处理器（见 examples/llm-bot）。
func WeComToolDefs() []ToolDef {
	return []ToolDef{
		{
			Name: "send_template_card",
			Description: "向用户当前所在的会话发送一张企业微信模板卡片（模板消息)，适合通知、" +
				"菜单、投票、数据展示等结构化信息。card 是完整的模板卡片 JSON：必须包含 card_type" +
				"（text_notice 文本通知 / news_notice 图文通知 / button_interaction 按钮交互 / " +
				"vote_interaction 投票 / multiple_interaction 多选），其余字段按企业微信智能机器人" +
				"模板卡片规范填写，常用如 main_title{title,desc}、sub_title_text、source{icon_url,desc}、" +
				"emphasis_content{title,desc}、card_action{url}、button_list[{text_url/text,key}]、" +
				"task_id（交互卡片必填，用于回调定位）、image_list、checkbox/radio option 等。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"card": map[string]any{
						"type":        "object",
						"description": "完整的模板卡片 JSON 结构",
					},
				},
				"required": []any{"card"},
			},
		},
		{
			Name: "send_markdown",
			Description: "向当前会话额外发送一条 markdown 消息（独立于流式回复）。仅用于补充" +
				"展示格式化长文本（表格、列表、多级标题等流式正文放不下的内容）。面向用户的" +
				"对话性回答仍应写在普通回复文本里，不要用这个工具替代普通回复。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{
						"type":        "string",
						"description": "markdown 正文，支持企微 markdown 子集",
					},
				},
				"required": []any{"content"},
			},
		},
	}
}

// NewWeComExecutor 构造绑定当前消息上下文的工具执行器：req 是本条消息的
// 回调载体，卡片优先原路回复（与流式帧天然串行）；chatID（由 req 提取）
// 可得时可作为主动发送的目标。两者都不可得则返回错误结果，模型可见。
func NewWeComExecutor(bot *wecomaibot.Client, req wecomaibot.RequestCarrier) ToolExecutor {
	chatID := extractChatID(req)
	return func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		switch name {
		case "send_template_card":
			return execSendTemplateCard(bot, req, chatID, args)
		case "send_markdown":
			return execSendMarkdown(bot, req, chatID, args)
		default:
			return "", fmt.Errorf("未知工具: %s", name)
		}
	}
}

// execSendTemplateCard 执行 send_template_card：解出卡片、校验 card_type，
// 优先原路回复（ReplyTemplateCard），回退主动发送（SendMessage）。
func execSendTemplateCard(bot *wecomaibot.Client, req wecomaibot.RequestCarrier, chatID string, argsRaw json.RawMessage) (string, error) {
	var args struct {
		Card wecomaibot.TemplateCard `json:"card"`
	}
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return toolResultJSON(false, 0, "参数解析失败: "+err.Error()), nil
	}
	if len(args.Card) == 0 || args.Card["card_type"] == "" {
		return toolResultJSON(false, 0, "card 缺少 card_type 字段"), nil
	}

	if req != nil {
		if _, err := bot.ReplyTemplateCard(req, args.Card, nil); err != nil {
			return toolResultJSON(false, 0, "发送卡片失败: "+err.Error()), nil
		}
		return toolResultJSON(true, 0, ""), nil
	}
	if chatID != "" {
		if _, err := bot.SendMessage(chatID, map[string]any{
			"msgtype":       "template_card",
			"template_card": args.Card,
		}); err != nil {
			return toolResultJSON(false, 0, "发送卡片失败: "+err.Error()), nil
		}
		return toolResultJSON(true, 0, ""), nil
	}
	return toolResultJSON(false, 0, "无法确定目标会话"), nil
}

// execSendMarkdown 执行 send_markdown：优先主动发送（SendMessage），
// chatID 不可得时回退普通回复帧。
func execSendMarkdown(bot *wecomaibot.Client, req wecomaibot.RequestCarrier, chatID string, argsRaw json.RawMessage) (string, error) {
	var args struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return toolResultJSON(false, 0, "参数解析失败: "+err.Error()), nil
	}
	if args.Content == "" {
		return toolResultJSON(false, 0, "content 不能为空"), nil
	}
	body := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": args.Content},
	}
	if chatID != "" {
		if _, err := bot.SendMessage(chatID, body); err != nil {
			return toolResultJSON(false, 0, "发送 markdown 失败: "+err.Error()), nil
		}
		return toolResultJSON(true, 0, ""), nil
	}
	if req != nil {
		if _, err := bot.Reply(req, body, wecomaibot.WsCmdResponse); err != nil {
			return toolResultJSON(false, 0, "发送 markdown 失败: "+err.Error()), nil
		}
		return toolResultJSON(true, 0, ""), nil
	}
	return toolResultJSON(false, 0, "无法确定目标会话"), nil
}

// toolResultJSON 拼回喂给模型的工具结果（ok/errmsg 的 JSON 字符串）。
func toolResultJSON(ok bool, errcode int, errmsg string) string {
	b, _ := json.Marshal(map[string]any{
		"ok":      ok,
		"errcode": errcode,
		"errmsg":  errmsg,
	})
	return string(b)
}
