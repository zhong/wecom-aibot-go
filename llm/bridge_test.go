package llm

import (
	"context"
	"encoding/json"
	"errors"
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

// setAckDelayMS 配置回复帧 ACK 的注入延迟：ACK 积压时 ReplyStreamNonBlocking
// 的 HasPendingAck 判定生效（流式帧在上一帧未 ACK 时跳发），供跳帧路径测试。
func (s *botWSServer) setAckDelayMS(ms int) {
	s.mu.Lock()
	s.ackDelayMS = ms
	s.mu.Unlock()
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

// templateCardFrames 返回卡片帧（aibot_respond_msg 里 msgtype 为
// template_card 的回复），并解出 card 内容，供工具执行断言。
func (s *botWSServer) templateCardFrames() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, f := range s.frames {
		if f.Cmd != "aibot_respond_msg" {
			continue
		}
		var body struct {
			MsgType      string         `json:"msgtype"`
			TemplateCard map[string]any `json:"template_card"`
		}
		if err := json.Unmarshal(f.Body, &body); err != nil || body.MsgType != "template_card" {
			continue
		}
		out = append(out, body.TemplateCard)
	}
	return out
}

func (s *botWSServer) url() string {
	return "ws" + strings.TrimPrefix(s.srv.URL, "http")
}

// fakeRound 是 fakeLLM 的一轮脚本：文本增量 + 本轮末尾产出的工具调用。
type fakeRound struct {
	deltas    []string
	toolCalls []ToolCall
	roundErr  error
}

// fakeLLM 实现 Client 接口：按 rounds 脚本轮次输出（每调用一次弹出最旧一轮），
// 兼容旧的单轮用法（deltas/streamErr 被视为唯一一轮）。记录收到的全部请求。
type fakeLLM struct {
	rounds       []fakeRound
	streamErr    error // rounds 为空时兜底注入的中途错误
	onDeltaCalls int
	mu           sync.Mutex
	gotReqs      []ChatRequest
}

// newSingleRoundLLM 构造旧式单轮 fake（既有 StreamReply 测试用）。
func newSingleRoundLLM(deltas []string, streamErr error) *fakeLLM {
	return &fakeLLM{rounds: []fakeRound{{deltas: deltas, roundErr: streamErr}}}
}

func (f *fakeLLM) StreamChat(ctx context.Context, req ChatRequest, onDelta func(string) error) (string, error) {
	return f.StreamChatEvents(ctx, req, eventsToDelta(onDelta))
}

func (f *fakeLLM) StreamChatEvents(_ context.Context, req ChatRequest, onEvent func(StreamEvent) error) (string, error) {
	f.mu.Lock()
	f.gotReqs = append(f.gotReqs, req)
	if len(f.rounds) == 0 {
		f.mu.Unlock()
		if f.streamErr != nil {
			return "", f.streamErr
		}
		return "stop", nil
	}
	round := f.rounds[0]
	f.rounds = f.rounds[1:]
	f.mu.Unlock()

	for _, d := range round.deltas {
		f.mu.Lock()
		f.onDeltaCalls++
		f.mu.Unlock()
		if err := onEvent(StreamEvent{Type: StreamEventTextDelta, TextDelta: d}); err != nil {
			return "", err
		}
	}
	for _, tc := range round.toolCalls {
		if err := onEvent(StreamEvent{Type: StreamEventToolCall, ToolCall: tc}); err != nil {
			return "", err
		}
	}
	if round.roundErr != nil {
		return "", round.roundErr
	}
	if len(round.toolCalls) > 0 {
		return "tool_calls", nil
	}
	return "stop", nil
}

// popReq 返回第 i 次收到的请求（供多轮断言）。
func (f *fakeLLM) popReq(i int) ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotReqs[i]
}

// reqCount 返回收到的请求总数。
func (f *fakeLLM) reqCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.gotReqs)
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
		client := newSingleRoundLLM([]string{"你好", "，", "世界"}, nil)
		streamID := wecomaibot.GenerateReqID("stream")
		final, err := StreamReply(context.Background(), bot, msg, streamID, client,
			ChatRequest{}, "正在思考...")
		if err != nil {
			t.Errorf("StreamReply 失败: %v", err)
		}
		done <- final
	})
	// 取回返回值供下方断言：先消费在测试体，恒走 default 的旧写法
	// 等于把这条断言判了死刑。
	final := <-done
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
	if final != "你好，世界" {
		t.Errorf("finalText = %q, 期望全文", final)
	}
}

func TestStreamReplyLLMErrorSendsErrorFinish(t *testing.T) {
	// LLM 中途出错且无内容：终帧应为兜底错误文案，流一定闭环。
	srv := newBotWSServer(t)
	done := make(chan struct{})
	connectBot(t, srv, func(bot *wecomaibot.Client, msg wecomaibot.TextMessage) {
		defer close(done)
		client := newSingleRoundLLM(nil, errAbort)
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
		client := newSingleRoundLLM(nil, nil)
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
		client := newSingleRoundLLM([]string{"前半"}, errAbort)
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

func TestAgentReplyCardAndFinalFrame(t *testing.T) {
	// 两轮脚本：第一轮流前置文本 + 发卡片工具，第二轮终稿文本。
	srv := newBotWSServer(t)
	card := map[string]any{
		"card_type":  "text_notice",
		"main_title": map[string]any{"title": "本周值班表", "desc": "值班安排"},
	}
	cardRaw, _ := json.Marshal(card)
	client := &fakeLLM{rounds: []fakeRound{
		{
			deltas:    []string{"正在生成值班卡片…"},
			toolCalls: []ToolCall{{ID: "call_1", Name: "send_template_card", Arguments: json.RawMessage(`{"card":` + string(cardRaw) + `}`)}},
		},
		{deltas: []string{"已发出，请查收。"}},
	}}
	done := make(chan string, 1)
	connectBot(t, srv, func(bot *wecomaibot.Client, msg wecomaibot.TextMessage) {
		streamID := wecomaibot.GenerateReqID("stream")
		final, err := AgentReply(context.Background(), bot, msg, streamID, client,
			ChatRequest{}, NewWeComExecutor(bot, msg), "正在思考...",
			TurnOptions{RoundNotice: "\n"})
		if err != nil {
			t.Errorf("AgentReply 失败: %v", err)
		}
		done <- final
	})
	final := <-done
	time.Sleep(200 * time.Millisecond)

	// 工具执行应原路发出一张卡片帧
	cards := srv.templateCardFrames()
	if len(cards) != 1 {
		t.Fatalf("应发出一张卡片, 实际 %d", len(cards))
	}
	if cards[0]["card_type"] != "text_notice" {
		t.Errorf("卡片 card_type 不对: %+v", cards[0]["card_type"])
	}
	// 终帧 = 占位替换后的累积全文（前置文本 + 轮间提示 + 终稿），且闭环
	frames := srv.streamFrames()
	if len(frames) < 2 {
		t.Fatalf("流式帧过少: %d", len(frames))
	}
	last := frames[len(frames)-1]
	want := "正在生成值班卡片…\n已发出，请查收。"
	if !last.Finish || last.Content != want {
		t.Errorf("终帧应为 finish=true 且携带全文 %q: %+v", want, last)
	}
	if final != want {
		t.Errorf("finalText = %q, 期望 %q", final, want)
	}
}

func TestAgentReplyFinalRoundOnly(t *testing.T) {
	// 终帧净化：FinalRoundOnly 开启时终帧只携带最后一轮正文；
	// 中间帧照常全程累积推送，不回退。
	srv := newBotWSServer(t)
	client := &fakeLLM{rounds: []fakeRound{
		{
			deltas:    []string{"正在生成值班卡片…"},
			toolCalls: []ToolCall{{ID: "call_1", Name: "send_template_card", Arguments: json.RawMessage(`{"card":{"card_type":"text_notice"}}`)}},
		},
		{deltas: []string{"已发出，请查收。"}},
	}}
	done := make(chan string, 1)
	connectBot(t, srv, func(bot *wecomaibot.Client, msg wecomaibot.TextMessage) {
		streamID := wecomaibot.GenerateReqID("stream")
		final, err := AgentReply(context.Background(), bot, msg, streamID, client,
			ChatRequest{}, NewWeComExecutor(bot, msg), "正在思考...",
			TurnOptions{FinalRoundOnly: true})
		if err != nil {
			t.Errorf("AgentReply 失败: %v", err)
		}
		done <- final
	})
	final := <-done
	time.Sleep(200 * time.Millisecond)

	frames := srv.streamFrames()
	if len(frames) < 3 {
		t.Fatalf("流式帧过少: %d", len(frames))
	}
	// 中间帧（占位之后）携带全程累积文本，不被净化截断
	if frames[1].Content != "正在生成值班卡片…" {
		t.Errorf("中间帧应携带第一轮累积文本: %+v", frames[1])
	}
	// 终帧只携带最后一轮正文
	last := frames[len(frames)-1]
	if !last.Finish || last.Content != "已发出，请查收。" {
		t.Errorf("终帧应只携带最后一轮正文: %+v", last)
	}
	if final != "已发出，请查收。" {
		t.Errorf("finalText = %q, 期望净化后的最后一轮文本", final)
	}
}

func TestAgentReplyFinalRoundOnlyEmptyLastRound(t *testing.T) {
	// 最后一轮为空（模型只在工具轮说过话）：终帧退回已推送的累积文本，
	// 不能比用户已见的更少。
	srv := newBotWSServer(t)
	client := &fakeLLM{rounds: []fakeRound{
		{
			deltas:    []string{"看一下，随后发卡片。"},
			toolCalls: []ToolCall{{ID: "call_1", Name: "send_template_card", Arguments: json.RawMessage(`{"card":{"card_type":"text_notice"}}`)}},
		},
		{}, // 第二轮无文本无工具
	}}
	done := make(chan string, 1)
	connectBot(t, srv, func(bot *wecomaibot.Client, msg wecomaibot.TextMessage) {
		streamID := wecomaibot.GenerateReqID("stream")
		final, err := AgentReply(context.Background(), bot, msg, streamID, client,
			ChatRequest{}, NewWeComExecutor(bot, msg), "正在思考...",
			TurnOptions{FinalRoundOnly: true})
		if err != nil {
			t.Errorf("AgentReply 失败: %v", err)
		}
		done <- final
	})
	final := <-done
	time.Sleep(200 * time.Millisecond)

	frames := srv.streamFrames()
	if len(frames) == 0 {
		t.Fatal("没有流式帧, 未闭环")
	}
	last := frames[len(frames)-1]
	if !last.Finish || last.Content != "看一下，随后发卡片。" {
		t.Errorf("末轮为空时终帧应退回累积文本: %+v", last)
	}
	if final != "看一下，随后发卡片。" {
		t.Errorf("finalText = %q", final)
	}
}

func TestAgentReplyErrorClosesStream(t *testing.T) {
	// 第一轮流中途出错：已有内容则带内容发终帧，流闭环且错误上抛。
	srv := newBotWSServer(t)
	client := &fakeLLM{rounds: []fakeRound{{deltas: []string{"前半"}, roundErr: errAbort}}}
	done := make(chan struct{})
	connectBot(t, srv, func(bot *wecomaibot.Client, msg wecomaibot.TextMessage) {
		defer close(done)
		streamID := wecomaibot.GenerateReqID("stream")
		_, err := AgentReply(context.Background(), bot, msg, streamID, client,
			ChatRequest{}, NewWeComExecutor(bot, msg), "", TurnOptions{})
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

// TestReplyStreamNonBlockingSkipsPendingAck 钉跳帧分支（reply.go
// NonBlocking：非 finish 帧若有上一帧未 ACK 则跳发，finish 帧必发）。
// 该分支被 llm/acp 两个 bridge 的生产路径每日使用，但此前零覆盖：
// 假服务端给回复帧 ACK 注入延迟制造积压，直接观察 skipped 语义。
func TestReplyStreamNonBlockingSkipsPendingAck(t *testing.T) {
	srv := newBotWSServer(t)
	srv.setAckDelayMS(500) // 第一帧的 ACK 迟到，制造 pending 窗口

	botCh := make(chan *wecomaibot.Client, 1)
	connectBot(t, srv, func(bot *wecomaibot.Client, msg wecomaibot.TextMessage) {
		botCh <- bot
	})
	var bot *wecomaibot.Client
	select {
	case bot = <-botCh:
	case <-time.After(3 * time.Second):
		t.Fatal("bot 未就绪")
	}
	msg := wecomaibot.TextMessage{}
	msg.ReqID = "req_nb_1"
	msg.ChatID = "chat_1"

	// Reply 是「发送并阻塞等 ACK」语义：同步发送返回时 ACK 已落账、
	// pending 已清。要在 pending 窗口内观察跳帧，须让首帧在途——
	// goroutine 发首帧，ACK 延迟窗口内主线程打 NonBlocking 帧。
	firstDone := make(chan error, 1)
	go func() {
		_, skipped, err := bot.ReplyStreamNonBlocking(msg, "sid_nb", "第一帧", false, nil, nil)
		if skipped {
			firstDone <- errors.New("无 pending 时首帧不应跳发")
			return
		}
		firstDone <- err
	}()
	// 等首帧进入 pendingAcks（发送立即登记，不等 ACK）
	pendingDeadline := time.Now().Add(2 * time.Second)
	for !bot.HasPendingAck("req_nb_1") && time.Now().Before(pendingDeadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !bot.HasPendingAck("req_nb_1") {
		t.Fatal("前置失败: 首帧应已在途等待 ACK")
	}
	// 在途窗口内：非结束帧应跳发
	_, skipped, err := bot.ReplyStreamNonBlocking(msg, "sid_nb", "第二帧", false, nil, nil)
	if err != nil {
		t.Fatalf("跳帧调用不应报错: %v", err)
	}
	if !skipped {
		t.Fatal("有 pending ACK 时非结束帧应跳发 (skipped=true)")
	}
	// 结束帧：pending 之下也必须发送
	_, skipped, err = bot.ReplyStreamNonBlocking(msg, "sid_nb", "全文", true, nil, nil)
	if err != nil {
		t.Fatalf("结束帧发送失败: %v", err)
	}
	if skipped {
		t.Fatal("结束帧 (finish=true) 不得跳发")
	}
	if err := <-firstDone; err != nil {
		t.Fatalf("首帧收尾: %v", err)
	}
}
