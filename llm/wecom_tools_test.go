package llm

import (
	"context"
	"encoding/json"
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
