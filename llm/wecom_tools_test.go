package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	wecomaibot "github.com/zhong/wecom-aibot-go"
)

func TestWeComExecutorReplyTemplateCard(t *testing.T) {
	// req 载体可得：send_template_card 应原路回复卡片帧。
	srv := newBotWSServer(t)
	done := make(chan string, 1)
	connectBot(t, srv, func(bot *wecomaibot.Client, msg wecomaibot.TextMessage) {
		exec := NewWeComExecutor(bot, msg)
		result, err := exec(context.Background(), "send_template_card",
			json.RawMessage(`{"card":{"card_type":"text_notice","main_title":{"title":"t"}}}`))
		if err != nil {
			t.Errorf("执行失败: %v", err)
		}
		done <- result
	})
	result := waitResult(t, done)
	time.Sleep(200 * time.Millisecond)

	if !strings.Contains(result, `"ok":true`) {
		t.Errorf("执行结果应为 ok: %s", result)
	}
	cards := srv.templateCardFrames()
	if len(cards) != 1 {
		t.Fatalf("应发出一张卡片, 实际 %d", len(cards))
	}
	if cards[0]["card_type"] != "text_notice" {
		t.Errorf("卡片 card_type 不对: %+v", cards[0])
	}
}

func TestWeComExecutorMarkdownOverReplay(t *testing.T) {
	// chatID 可得（假推送 chatid=chat_1）：send_markdown 走 SendMessage 主动发送。
	srv := newBotWSServer(t)
	done := make(chan string, 1)
	connectBot(t, srv, func(bot *wecomaibot.Client, msg wecomaibot.TextMessage) {
		exec := NewWeComExecutor(bot, msg)
		result, err := exec(context.Background(), "send_markdown",
			json.RawMessage(`{"content":"## 名单\n- 张三"}`))
		if err != nil {
			t.Errorf("执行失败: %v", err)
		}
		done <- result
	})
	result := waitResult(t, done)
	time.Sleep(200 * time.Millisecond)

	if !strings.Contains(result, `"ok":true`) {
		t.Errorf("执行结果应为 ok: %s", result)
	}
	frames := srv.sendMsgFrames()
	if len(frames) != 1 {
		t.Fatalf("应有一条主动发送帧, 实际 %d", len(frames))
	}
	var body struct {
		ChatID   string `json:"chatid"`
		MsgType  string `json:"msgtype"`
		Markdown struct {
			Content string `json:"content"`
		} `json:"markdown"`
	}
	if err := json.Unmarshal(frames[0].Body, &body); err != nil {
		t.Fatalf("解析发送帧失败: %v", err)
	}
	if body.ChatID != "chat_1" || body.MsgType != "markdown" || !strings.Contains(body.Markdown.Content, "张三") {
		t.Errorf("markdown 主动发送帧不对: %+v", body)
	}
}

func TestWeComExecutorReadAttachment(t *testing.T) {
	// 文本类附件（扩展名 .txt）：read_attachment 返回内容预览 JSON。
	srv := newBotWSServer(t)
	rec := make(chan string, 1)
	connectBot(t, srv, func(bot *wecomaibot.Client, msg wecomaibot.TextMessage) {
		exec := NewWeComExecutor(bot, msg)
		r, err := exec(context.Background(), "read_attachment", json.RawMessage(`{}`))
		if err != nil {
			t.Errorf("执行失败: %v", err)
		}
		rec <- r
	})
	// 直接对携带附件的 FileMessage 载体执行（下载经 HTTP）。
	file, _ := os.CreateTemp(t.TempDir(), "名单-*.txt")
	_, _ = file.WriteString("张三,李四,王五")
	_ = file.Close()
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := os.ReadFile(file.Name())
		_, _ = w.Write(b)
	}))
	t.Cleanup(dlSrv.Close)

	bot2, err := wecomaibot.NewClient(wecomaibot.Config{
		BotID: "bot", Secret: "secret", WSURL: srv.url(),
		HeartbeatIntervalMS: 60000, Logger: silentLogger{},
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	fmsg := wecomaibot.FileMessage{}
	fmsg.ReqID = "req_1"
	fmsg.File.URL = dlSrv.URL
	fmsg.File.AESKey = ""
	exec2 := NewWeComExecutor(bot2, fmsg)
	r2, err := exec2(context.Background(), "read_attachment", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	var parsed struct {
		Ok      bool   `json:"ok"`
		Type    string `json:"type"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(r2), &parsed); err != nil {
		t.Fatalf("解析结果失败: %v", err)
	}
	if !parsed.Ok || parsed.Type != "text" || !strings.Contains(parsed.Content, "张三") {
		t.Errorf("文本附件应返回内容预览: %s", r2)
	}

	// 文本消息无附件：明确告知模型
	r1 := <-rec
	if !strings.Contains(r1, "没有附件") {
		t.Errorf("无附件应明确返回: %s", r1)
	}
}

func TestWeComExecutorValidationErrors(t *testing.T) {
	srv := newBotWSServer(t)
	done := make(chan string, 3)
	connectBot(t, srv, func(bot *wecomaibot.Client, msg wecomaibot.TextMessage) {
		exec := NewWeComExecutor(bot, msg)
		r1, _ := exec(context.Background(), "send_template_card", json.RawMessage(`{"card":{}}`))
		r2, _ := exec(context.Background(), "send_template_card", json.RawMessage(`not-json`))
		r3, err3 := exec(context.Background(), "unknown_tool", json.RawMessage(`{}`))
		done <- r1
		done <- r2
		done <- r3 + "|" + err3.Error()
	})
	r1, r2, r3 := waitResult(t, done), waitResult(t, done), waitResult(t, done)

	if !strings.Contains(r1, "card_type") {
		t.Errorf("缺 card_type 应报错: %s", r1)
	}
	if !strings.Contains(r2, "参数解析失败") {
		t.Errorf("非法参数应报错: %s", r2)
	}
	if !strings.Contains(r3, "未知工具") {
		t.Errorf("未知工具应报错: %s", r3)
	}
}

// waitResult 从 done 通道取执行结果（小工具，避免重复 select 样板）。
func waitResult(t *testing.T, done chan string) string {
	t.Helper()
	select {
	case s := <-done:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("等待执行结果超时")
		return ""
	}
}

// TestQuotedAttachmentReadAttachment 群聊里独立图片不产生回调，用户以
// 「引用消息 @机器人」（msgtype=text + quote 载被引用内容）送达附件：
// read_attachment 应把 quote 里的图片/文件/混排图片当本条消息的附件下载。
func TestQuotedAttachmentReadAttachment(t *testing.T) {
	// 引用图片：二进制附件 → 返回元信息（type=image）
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("\x89PNG fake image bytes"))
	}))
	t.Cleanup(dlSrv.Close)

	bot, err := wecomaibot.NewClient(wecomaibot.Config{
		BotID: "bot", Secret: "secret", WSURL: newBotWSServer(t).url(),
		HeartbeatIntervalMS: 60000, Logger: silentLogger{},
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}

	msg := wecomaibot.TextMessage{}
	msg.ReqID = "req_q1"
	msg.ChatID = "chat_1"
	msg.Text.Content = "这张图里是什么"
	msg.Quote = &wecomaibot.QuoteContent{MsgType: "image",
		Image: &wecomaibot.ImageContent{URL: dlSrv.URL, AESKey: ""}}
	exec := NewWeComExecutor(bot, msg)
	r, err := exec(context.Background(), "read_attachment", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	var meta struct {
		Ok   bool   `json:"ok"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(r), &meta); err != nil {
		t.Fatalf("解析结果失败: %v（%s）", err, r)
	}
	if !meta.Ok || meta.Type != "image" {
		t.Errorf("引用图片应按图片附件返回元信息: %s", r)
	}

	// 引用文件（文本类）：返回内容预览
	dlSrv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("张三,李四"))
	}))
	t.Cleanup(dlSrv2.Close)
	fq := wecomaibot.TextMessage{}
	fq.ReqID = "req_q2"
	fq.ChatID = "chat_1"
	fq.Text.Content = "统计这个文件"
	fq.Quote = &wecomaibot.QuoteContent{MsgType: "file",
		File: &wecomaibot.FileContent{URL: dlSrv2.URL, AESKey: ""}}
	r2, err := NewWeComExecutor(bot, fq)(context.Background(), "read_attachment", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(r2, "张三") {
		t.Errorf("引用文件（文本类）应返回内容预览: %s", r2)
	}

	// 普通文本消息（无引用 / 引用纯文本 / 引用但无附件载荷）→ 没有附件
	plain := wecomaibot.TextMessage{}
	plain.ReqID = "req_q3"
	plain.Text.Content = "你好"
	r3, _ := NewWeComExecutor(bot, plain)(context.Background(), "read_attachment", json.RawMessage(`{}`))
	if !strings.Contains(r3, "没有附件") {
		t.Errorf("无引用文本不应有附件: %s", r3)
	}
	quoteText := plain
	quoteText.Quote = &wecomaibot.QuoteContent{MsgType: "text",
		Text: &wecomaibot.TextContent{Content: "被引用的文字"}}
	r4, _ := NewWeComExecutor(bot, quoteText)(context.Background(), "read_attachment", json.RawMessage(`{}`))
	if !strings.Contains(r4, "没有附件") {
		t.Errorf("引用纯文本不应有附件: %s", r4)
	}
}

// TestReadAttachmentSaveToDir read_attachment 的 save=true 落盘路径：
// 二进制附件（PNG 头）保存到 saveDir，结果带 saved 绝对路径与明确 note；
// save=false / 无参数时行为不变（只有元信息、无 saved 字段）。
func TestReadAttachmentSaveToDir(t *testing.T) {
	bot, err := wecomaibot.NewClient(wecomaibot.Config{
		BotID: "bot", Secret: "secret", WSURL: newBotWSServer(t).url(),
		HeartbeatIntervalMS: 60000, Logger: silentLogger{},
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(png)
	}))
	t.Cleanup(dlSrv.Close)

	msg := wecomaibot.TextMessage{}
	msg.ReqID = "req_s1"
	msg.ChatID = "chat_1"
	msg.Text.Content = "看下这张图"
	msg.Quote = &wecomaibot.QuoteContent{MsgType: "image",
		Image: &wecomaibot.ImageContent{URL: dlSrv.URL}}
	saveDir := t.TempDir()

	// save=true：落盘 + saved 路径
	exec := NewWeComExecutorCwd(bot, msg, saveDir)
	r, err := exec(context.Background(), "read_attachment", json.RawMessage(`{"save":true}`))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	var saved struct {
		Ok    bool   `json:"ok"`
		Type  string `json:"type"`
		Saved string `json:"saved"`
	}
	if err := json.Unmarshal([]byte(r), &saved); err != nil {
		t.Fatalf("解析结果失败: %v（%s）", err, r)
	}
	if !saved.Ok || saved.Type != "image" || saved.Saved == "" {
		t.Fatalf("save=true 应返回 saved 路径: %s", r)
	}
	buf, err := os.ReadFile(saved.Saved)
	if err != nil || !bytes.HasPrefix(buf, []byte("\x89PNG")) {
		t.Errorf("落盘文件应可读且为 PNG 内容: path=%s err=%v", saved.Saved, err)
	}
	if !strings.Contains(r, "已保存为本地文件") {
		t.Errorf("note 应说明已落盘: %s", r)
	}

	// save 不传：行为不变，无 saved 字段
	r2, err := exec(context.Background(), "read_attachment", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if strings.Contains(r2, "saved") {
		t.Errorf("未请求 save 不应落盘: %s", r2)
	}

	// saveDir 为空（llm 模式）：save=true 也只有元信息，不报错
	exec2 := NewWeComExecutor(bot, msg)
	r3, err := exec2(context.Background(), "read_attachment", json.RawMessage(`{"save":true}`))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if strings.Contains(r3, "saved") {
		t.Errorf("saveDir 为空不应有 saved: %s", r3)
	}

	// 文本类附件不受 save 影响（内容预览就是全文）
	fdlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("张三,李四"))
	}))
	t.Cleanup(fdlSrv.Close)
	fq := msg
	fq.Quote = &wecomaibot.QuoteContent{MsgType: "file",
		File: &wecomaibot.FileContent{URL: fdlSrv.URL}}
	r4, err := NewWeComExecutorCwd(bot, fq, saveDir)(context.Background(), "read_attachment", json.RawMessage(`{"save":true}`))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(r4, "张三") || strings.Contains(r4, "saved") {
		t.Errorf("文本类附件应照常内容预览: %s", r4)
	}
}
