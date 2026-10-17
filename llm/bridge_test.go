package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	wecomaibot "github.com/zhong/wecom-aibot-go"
)

// silentLogger 静默日志器（根包测试同款）。
type silentLogger struct{}

func (silentLogger) Debug(string, ...any) {}
func (silentLogger) Info(string, ...any)  {}
func (silentLogger) Warn(string, ...any)  {}
func (silentLogger) Error(string, ...any) {}

// botWSServer 是本包内的假 openws 服务端（对齐根包 reply_test.go 的
// callbackServer 模式）：aibot_subscribe ACK 并推送一条文本消息回调，
// 其余帧一律 ACK。ackDelayMS 可配置 ACK 延迟，用于触发 NonBlocking 跳帧路径。
type botWSServer struct {
	srv        *httptest.Server
	ackDelayMS int

	mu     sync.Mutex
	frames []wecomaibot.WsFrameRaw
}

func newBotWSServer(t *testing.T) *botWSServer {
	t.Helper()
	s := &botWSServer{}
	upgrader := websocket.Upgrader{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var frame wecomaibot.WsFrameRaw
			if err := json.Unmarshal(data, &frame); err != nil {
				return
			}
			s.record(frame)

			if strings.HasPrefix(frame.Headers.ReqID, "aibot_subscribe_") {
				_ = conn.WriteJSON(wecomaibot.WsFrameRaw{Headers: wecomaibot.WsHeaders{ReqID: frame.Headers.ReqID}})
				_ = conn.WriteJSON(map[string]any{
					"cmd":     "aibot_msg_callback",
					"headers": map[string]string{"req_id": "msg_req_1"},
					"body": map[string]any{
						"msgid":    "m1",
						"aibotid":  "bot",
						"chatid":   "chat_1",
						"chattype": "group",
						"from":     map[string]string{"userid": "u1"},
						"msgtype":  "text",
						"text":     map[string]string{"content": "hi"},
					},
				})
				continue
			}
			// 回复/主动发送帧一律 ACK（可注入延迟制造积压）
			s.mu.Lock()
			delay := s.ackDelayMS
			s.mu.Unlock()
			if delay > 0 {
				time.Sleep(time.Duration(delay) * time.Millisecond)
			}
			_ = conn.WriteJSON(wecomaibot.WsFrameRaw{Headers: wecomaibot.WsHeaders{ReqID: frame.Headers.ReqID}})
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *botWSServer) record(frame wecomaibot.WsFrameRaw) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, frame)
}

// streamFrames 依序返回 body.stream 内容非空的帧（流式回复帧）。
func (s *botWSServer) streamFrames() []wecomaibot.StreamPayload {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []wecomaibot.StreamPayload
	for _, f := range s.frames {
		if f.Cmd != "aibot_respond_msg" {
			continue
		}
		var body struct {
			Stream wecomaibot.StreamPayload `json:"stream"`
		}
		if err := json.Unmarshal(f.Body, &body); err != nil || body.Stream.ID == "" {
			continue
		}
		sp := body.Stream
		out = append(out, sp)
	}
	return out
}

// sendMsgFrames 返回主动发送降级帧。
func (s *botWSServer) sendMsgFrames() []wecomaibot.WsFrameRaw {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []wecomaibot.WsFrameRaw
	for _, f := range s.frames {
		if f.Cmd == "aibot_send_msg" {
			out = append(out, f)
		}
	}
	return out
}

func (s *botWSServer) url() string {
	return "ws" + strings.TrimPrefix(s.srv.URL, "http")
}

// fakeLLM 实现 Client 接口，按脚本输出增量；可注入中途错误。
type fakeLLM struct {
	deltas       []string
	streamErr    error
	onDeltaCalls int
}

func (f *fakeLLM) StreamChat(_ context.Context, _ ChatRequest, onDelta func(string) error) (string, error) {
	for _, d := range f.deltas {
		f.onDeltaCalls++
		if err := onDelta(d); err != nil {
			return "", err
		}
	}
	if f.streamErr != nil {
		return "", f.streamErr
	}
	return "stop", nil
}

// connectBot 启动假服务端 + 客户端连接，收到推送的文本消息后把 bot 与消息
// 交给回调执行 bridge 用例。
func connectBot(t *testing.T, srv *botWSServer, run func(bot *wecomaibot.Client, msg wecomaibot.TextMessage)) {
	t.Helper()
	bot, err := wecomaibot.NewClient(wecomaibot.Config{
		BotID:               "bot",
		Secret:              "secret",
		WSURL:               srv.url(),
		HeartbeatIntervalMS: 60000,
		Logger:              silentLogger{},
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}

	bot.OnText(func(_ context.Context, msg wecomaibot.TextMessage) {
		defer func() {
			// 回调跑在独立 goroutine，panic 不能拖垮测试进程
			if rec := recover(); rec != nil {
				t.Errorf("处理回调 panic: %v", rec)
			}
		}()
		run(bot, msg)
	})

	if err := bot.Connect(); err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	t.Cleanup(bot.Disconnect)
}

func TestStreamReplyHappyPath(t *testing.T) {
	srv := newBotWSServer(t)
	done := make(chan string, 1)
	connectBot(t, srv, func(bot *wecomaibot.Client, msg wecomaibot.TextMessage) {
		client := &fakeLLM{deltas: []string{"你好", "，", "世界"}}
		streamID := wecomaibot.GenerateReqID("stream")
		final, err := StreamReply(context.Background(), bot, msg, streamID, client,
			ChatRequest{}, "正在思考...")
		if err != nil {
			t.Errorf("StreamReply 失败: %v", err)
		}
		done <- final
	})
	<-done
	// 等服务端把 ACK 落账
	time.Sleep(200 * time.Millisecond)

	frames := srv.streamFrames()
	if len(frames) < 3 {
		t.Fatalf("流式帧过少: %d", len(frames))
	}
	// 首帧是占位符，非 finish
	if frames[0].Content != "正在思考..." || frames[0].Finish {
		t.Errorf("首帧应为占位且非结束帧: %+v", frames[0])
	}
	// 中间帧携带累积全文
	if frames[1].Content != "你好" {
		t.Errorf("第二帧应为累积内容由小到大: %q", frames[1].Content)
	}
	// 终帧 finish=true 且为全文
	last := frames[len(frames)-1]
	if !last.Finish || last.Content != "你好，世界" {
		t.Errorf("终帧应为 finish=true 且携带全文: %+v", last)
	}
	if last.ID != frames[0].ID {
		t.Errorf("同一流的 streamID 应一致: %q vs %q", last.ID, frames[0].ID)
	}
	// 返回的 finalText 供多轮历史补记，应等于终帧全文
	select {
	case final := <-done:
		if final != "你好，世界" {
			t.Errorf("finalText = %q, 期望全文", final)
		}
	default:
	}
}

func TestStreamReplyLLMErrorSendsErrorFinish(t *testing.T) {
	// LLM 中途出错且无内容：终帧应为兜底错误文案，流一定闭环。
	srv := newBotWSServer(t)
	done := make(chan struct{})
	connectBot(t, srv, func(bot *wecomaibot.Client, msg wecomaibot.TextMessage) {
		defer close(done)
		client := &fakeLLM{deltas: nil, streamErr: errAbort}
		streamID := wecomaibot.GenerateReqID("stream")
		_, err := StreamReply(context.Background(), bot, msg, streamID, client,
			ChatRequest{}, "")
		if !errorsIs(err, errAbort) {
			t.Errorf("LLM 错误应向上传播, 实际: %v", err)
		}
	})
	<-done
	time.Sleep(200 * time.Millisecond)

	frames := srv.streamFrames()
	if len(frames) == 0 {
		t.Fatal("没有流式帧, 未闭环")
	}
	last := frames[len(frames)-1]
	if !last.Finish || last.Content != DefaultErrorText {
		t.Errorf("终帧应为兜底错误文案: %+v", last)
	}
}

func TestStreamReplyEmptyResponse(t *testing.T) {
	// 模型空回复：终帧用 DefaultEmptyText 兜底，不报错。
	srv := newBotWSServer(t)
	done := make(chan struct{})
	connectBot(t, srv, func(bot *wecomaibot.Client, msg wecomaibot.TextMessage) {
		defer close(done)
		client := &fakeLLM{deltas: nil}
		streamID := wecomaibot.GenerateReqID("stream")
		_, err := StreamReply(context.Background(), bot, msg, streamID, client,
			ChatRequest{}, "思考中")
		if err != nil {
			t.Errorf("空回复不应报错: %v", err)
		}
	})
	<-done
	time.Sleep(200 * time.Millisecond)

	frames := srv.streamFrames()
	if len(frames) < 2 {
		t.Fatalf("至少应有占位 + 终帧: %d", len(frames))
	}
	last := frames[len(frames)-1]
	if !last.Finish || last.Content != DefaultEmptyText {
		t.Errorf("终帧应为空回复兜底文案: %+v", last)
	}
}

func TestStreamReplyLLMErrorKeepsPartialContent(t *testing.T) {
	// LLM 中途出错但已有半截内容：终帧带现有内容而非错误文案。
	srv := newBotWSServer(t)
	done := make(chan struct{})
	connectBot(t, srv, func(bot *wecomaibot.Client, msg wecomaibot.TextMessage) {
		defer close(done)
		client := &fakeLLM{deltas: []string{"前半"}, streamErr: errAbort}
		streamID := wecomaibot.GenerateReqID("stream")
		_, err := StreamReply(context.Background(), bot, msg, streamID, client,
			ChatRequest{}, "")
		if !errorsIs(err, errAbort) {
			t.Errorf("LLM 错误应向上传播, 实际: %v", err)
		}
	})
	<-done
	time.Sleep(200 * time.Millisecond)

	frames := srv.streamFrames()
	if len(frames) == 0 {
		t.Fatal("没有流式帧, 未闭环")
	}
	last := frames[len(frames)-1]
	if !last.Finish || last.Content != "前半" {
		t.Errorf("终帧应带半截内容: %+v", last)
	}
}

func TestExtractChatID(t *testing.T) {
	base := wecomaibot.BaseMessage{ChatID: "chat_9"}
	if got := extractChatID(wecomaibot.TextMessage{BaseMessage: base}); got != "chat_9" {
		t.Errorf("TextMessage ChatID = %q", got)
	}
	if got := extractChatID(wecomaibot.VoiceMessage{BaseMessage: base}); got != "chat_9" {
		t.Errorf("VoiceMessage ChatID = %q", got)
	}
	if got := extractChatID(wecomaibot.EventMessage{ChatID: "chat_8"}); got != "chat_8" {
		t.Errorf("EventMessage ChatID = %q", got)
	}
	// 无法识别的载体返回空串，表示无法降级发送
	if got := extractChatID(unknownCarrier{}); got != "" {
		t.Errorf("未知载体应返回空串, 实际 %q", got)
	}
}

type unknownCarrier struct{}

func (unknownCarrier) RequestID() string { return "r" }
