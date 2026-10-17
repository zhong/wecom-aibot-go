package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	wecomaibot "github.com/zhong/wecom-aibot-go"
)

// WeComToolDefs 返回企微交互能力的工具目录，连同 JSON Schema 一起发给
// 模型，由模型自行决定何时调用。目标会话由 NewWeComExecutor 的闭包固定，
// 模型只能选择工具与内容。
//
// 工具描述按「内容形态 → 交互形式」给出自主选型指引，模型不依赖用户
// 显式指令也应能选出合适的交互形式（见每个工具的 Description）。
//
// 有意不在目录内的能力（与工具执行的时序/能力约束有关）：
//   - update_template_card：template_card_event 后只有 5 秒更新窗口，
//     一次 LLM 往返不可达；卡片更新应走确定性的 OnTemplateCardEvent
//     处理器（见 examples/llm-bot）。
//   - reply_welcome：enter_chat 事件后同样只有 5 秒欢迎语窗口。
//   - send_text：与普通流式回复重复，对话性内容直接写在回复文本里即可。
func WeComToolDefs() []ToolDef {
	return []ToolDef{
		{
			Name: "send_template_card",
			Description: "向当前会话发送一张企业微信模板卡片。选型指引：回答里包含以下形态之一时，" +
				"优先用本工具而不是纯文本——(1) 通知/公告类信息（如值班表、日程提醒、天气预警）；" +
				"(2) 数据展示（指标、清单、对比项）；(3) 需要用户选择或操作的交互（按钮、投票、" +
				"多选，用 button_interaction / vote_interaction / multiple_interaction）。" +
				"用户没有明确说\"发卡片\"也应该自主用卡片呈现这类内容。card 是完整的模板卡片 JSON：" +
				"必须包含 card_type（text_notice 文本通知 / news_notice 图文通知 / " +
				"button_interaction 按钮交互 / vote_interaction 投票 / multiple_interaction 多选），" +
				"其余字段按企业微信智能机器人模板卡片规范填写，常用如 main_title{title,desc}、" +
				"sub_title_text、source{icon_url,desc}、emphasis_content{title,desc}、" +
				"card_action{url}、button_list[{text_url/text,key}]、task_id（交互卡片必填，" +
				"用于回调定位）、image_list、checkbox/radio option 等。发完卡片后，" +
				"用普通回复文本简短说明即可，不要重复卡片内容。",
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
			Description: "向当前会话额外发送一条 markdown 消息（独立于流式回复正文）。" +
				"选型指引：仅当回答包含大段格式化长文本——表格、多层列表、多级标题、" +
				"代码块等在聊天正文中展示效果差的内容——时使用。" +
				"对话性回答、简短说明、一两句话能说清的内容一律直接写在普通回复文本里，" +
				"不要用这个工具替代普通回复，也不要先发一条普通回复再原样重发一遍 markdown。",
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
		{
			Name: "read_attachment",
			Description: "读取用户在本条消息里发来的附件内容（文件/图片/语音/视频/图文混排）。" +
				"选型指引：只要当前消息携带了附件且内容对回答有必要——例如用户发来一份名单" +
				"文件让你统计、或发文档让你总结——就先调用本工具拿到附件内容再作答，" +
				"不要凭空猜测附件内容。文本类附件（txt/csv/md/json 等）返回文本内容预览；" +
				"图片/音视频等二进制附件返回文件名与大小等元信息。无需参数：读取目标由" +
				"当前消息决定。",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
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
		case "read_attachment":
			return execReadAttachment(ctx, bot, req)
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

// readAttachmentPreviewLimit 是文本类附件返回给模型的内容预览上限（字节）：
// 既够模型读到关键内容，又不至于撑爆上下文。
const readAttachmentPreviewLimit = 8 * 1024

// readAttachmentMaxSize 是允许下载的附件大小上限（字节），超过直接告知模型。
const readAttachmentMaxSize = 10 * 1024 * 1024

// execReadAttachment 执行 read_attachment：从 req 载体提取附件（url+aeskey），
// 经 DownloadFile 下载（aeskey 可得时 AES 解密）。文本类返回截断内容预览，
// 二进制（图片/音视频等）返回文件名与大小元信息；无附件返回明确结果。
func execReadAttachment(ctx context.Context, bot *wecomaibot.Client, req wecomaibot.RequestCarrier) (string, error) {
	att, ok := attachmentOf(req)
	if !ok {
		return toolResultJSON(false, 0, "当前消息没有附件"), nil
	}
	downloaded, err := bot.DownloadFile(att.url, att.aesKey)
	if err != nil {
		return toolResultJSON(false, 0, "下载附件失败: "+err.Error()), nil
	}
	if len(downloaded.Buffer) > readAttachmentMaxSize {
		return toolResultJSON(false, 0,
			fmt.Sprintf("附件过大（%d 字节，超过 %d MiB 上限）", len(downloaded.Buffer), readAttachmentMaxSize/1024/1024)), nil
	}
	if isTextual(downloaded.Filename, att.msgType) || sniffTextual(downloaded.Buffer) {
		preview := string(downloaded.Buffer)
		truncated := false
		if len(preview) > readAttachmentPreviewLimit {
			preview = preview[:readAttachmentPreviewLimit]
			truncated = true
		}
		if truncated {
			preview += "\n…（内容已截断）"
		}
		return attachmentTextJSON(downloaded.Filename, len(downloaded.Buffer), preview, truncated), nil
	}
	return attachmentMetaJSON(downloaded.Filename, att.msgType, len(downloaded.Buffer)), nil
}

// reqAttachment 是从消息载体提取出的单个附件。
type reqAttachment struct {
	url     string
	aesKey  string
	msgType string // 附件来源的消息类型（file/image/voice/video/mixed）
}

// attachmentOf 判断消息是否携带附件并提取 url/aeskey。图文混排取首个
// 携带 url 的项；同一消息的多附件场景取第一项并在结果里说明。
func attachmentOf(req wecomaibot.RequestCarrier) (*reqAttachment, bool) {
	switch m := req.(type) {
	case wecomaibot.FileMessage:
		if m.File.URL == "" {
			return nil, false
		}
		return &reqAttachment{url: m.File.URL, aesKey: m.File.AESKey, msgType: "file"}, true
	case wecomaibot.ImageMessage:
		if m.Image.URL == "" {
			return nil, false
		}
		return &reqAttachment{url: m.Image.URL, aesKey: m.Image.AESKey, msgType: "image"}, true
	case wecomaibot.VoiceMessage:
		// 语音已被企微转写为 Text.Content 直接可读，无附件可下载。
		return nil, false
	case wecomaibot.VideoMessage:
		if m.Video.URL == "" {
			return nil, false
		}
		return &reqAttachment{url: m.Video.URL, aesKey: m.Video.AESKey, msgType: "video"}, true
	case wecomaibot.MixedMessage:
		for _, item := range m.Mixed.MsgItem {
			if item.Image != nil && item.Image.URL != "" {
				return &reqAttachment{url: item.Image.URL, aesKey: item.Image.AESKey, msgType: "mixed"}, true
			}
		}
	}
	return nil, false
}

// textualExts 是按扩展名判定的文本类附件。
var textualExts = map[string]bool{
	".txt": true, ".md": true, ".csv": true, ".json": true, ".xml": true,
	".yaml": true, ".yml": true, ".log": true, ".html": true, ".sql": true,
	".go": true, ".py": true, ".js": true, ".ts": true, ".java": true, ".sh": true,
}

// isTextual 判断附件是否按文本类内容预览返回：文件名扩展名属于文本类。
// 文件名缺失（服务端未回 Content-Disposition）时由 sniffTextual 兜底。
func isTextual(filename, msgType string) bool {
	if msgType != "file" && msgType != "mixed" {
		return false
	}
	i := strings.LastIndex(filename, ".")
	return i >= 1 && textualExts[strings.ToLower(filename[i:])]
}

// sniffTextual 嗅探字节是否像 UTF-8 可打印文本（无扩展名附件的兜底判定）：
// 取前 512 字节，无 NUL 字节、不含非法 UTF-8 序列即可接受。
func sniffTextual(b []byte) bool {
	head := b
	if len(head) > 512 {
		head = head[:512]
	}
	if len(head) == 0 {
		return false
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return false
	}
	return utf8.Valid(head)
}

// attachmentTextJSON 拼文本类附件的返回：文件名、总大小、内容预览。
func attachmentTextJSON(filename string, size int, preview string, truncated bool) string {
	b, _ := json.Marshal(map[string]any{
		"ok":        true,
		"type":      "text",
		"filename":  filename,
		"size":      size,
		"content":   preview,
		"truncated": truncated,
	})
	return string(b)
}

// attachmentMetaJSON 拼二进制附件的元信息返回：模型据此说明收到了什么。
func attachmentMetaJSON(filename, msgType string, size int) string {
	b, _ := json.Marshal(map[string]any{
		"ok":       true,
		"type":     msgType,
		"filename": filename,
		"size":     size,
		"note":     "二进制内容，请依据消息类型（图片/语音/视频）与文件名等信息作答",
	})
	return string(b)
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
