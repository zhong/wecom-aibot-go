package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// recordingExec 记录收到的工具调用并返回固定结果。
type recordingExec struct {
	mu     sync.Mutex
	calls  []ToolCall
	result string
	err    error
}

func (e *recordingExec) exec(_ context.Context, name string, args json.RawMessage) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, ToolCall{ID: "", Name: name, Arguments: args})
	if e.err != nil {
		return "", e.err
	}
	return e.result, nil
}

func (e *recordingExec) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

func TestRunTurnExecutesToolsAndLoops(t *testing.T) {
	client := &fakeLLM{rounds: []fakeRound{
		{
			deltas:    []string{"先看下值班安排…"},
			toolCalls: []ToolCall{{ID: "call_1", Name: "send_template_card", Arguments: json.RawMessage(`{"card":{"card_type":"text_notice"}}`)}},
		},
		{deltas: []string{"卡片已发出。"}},
	}}
	exec := &recordingExec{result: `{"ok":true}`}

	var got []string
	final, err := RunTurn(context.Background(), client, ChatRequest{}, exec.exec,
		func(d string) error { got = append(got, d); return nil }, TurnOptions{})
	if err != nil {
		t.Fatalf("RunTurn 失败: %v", err)
	}
	if final != "先看下值班安排…卡片已发出。" {
		t.Errorf("finalText = %q", final)
	}
	if strings.Join(got, "") != final {
		t.Errorf("onDelta 序列应与 finalText 一致: %q", strings.Join(got, ""))
	}

	// 第二轮请求应包含 assistant 工具调用 + role "tool" 回喂结果
	if client.reqCount() != 2 {
		t.Fatalf("应有两次流式请求, 实际 %d", client.reqCount())
	}
	second := client.popReq(1)
	n := len(second.Messages)
	if n < 2 {
		t.Fatalf("第二轮消息过少: %+v", second.Messages)
	}
	asst := second.Messages[n-2]
	if asst.Role != "assistant" || asst.Content != "先看下值班安排…" {
		t.Errorf("assistant 回喂消息不对: %+v", asst)
	}
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call_1" {
		t.Errorf("assistant 应携带 tool_calls: %+v", asst.ToolCalls)
	}
	toolMsg := second.Messages[n-1]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call_1" || toolMsg.Content != `{"ok":true}` {
		t.Errorf("tool 回喂消息不对: %+v", toolMsg)
	}
	if exec.callCount() != 1 {
		t.Errorf("执行器应被调用一次, 实际 %d", exec.callCount())
	}
	// 执行器收到的应是原始参数 JSON
	if string(exec.calls[0].Arguments) != `{"card":{"card_type":"text_notice"}}` {
		t.Errorf("执行器收到参数不对: %s", exec.calls[0].Arguments)
	}
}

func TestRunTurnMaxRoundsExceeded(t *testing.T) {
	round := fakeRound{toolCalls: []ToolCall{{ID: "c", Name: "send_markdown", Arguments: json.RawMessage(`{"content":"x"}`)}}}
	client := &fakeLLM{rounds: []fakeRound{round, round, round, round}} // 永远调工具
	exec := &recordingExec{result: `{"ok":true}`}

	final, err := RunTurn(context.Background(), client, ChatRequest{}, exec.exec,
		func(string) error { return nil }, TurnOptions{MaxToolRounds: 2})
	if !errors.Is(err, ErrMaxToolRounds) {
		t.Errorf("期望 ErrMaxToolRounds, 实际: %v", err)
	}
	if final != "" {
		t.Errorf("无文本产出时 finalText 应为空, 实际 %q", final)
	}
	// MaxToolRounds=2：两次流式调用后即收场，不再发第三轮
	if client.reqCount() != 2 {
		t.Errorf("应恰好两次流式请求, 实际 %d", client.reqCount())
	}
}

func TestRunTurnExecutorErrorFedBack(t *testing.T) {
	client := &fakeLLM{rounds: []fakeRound{
		{toolCalls: []ToolCall{{ID: "c1", Name: "send_markdown", Arguments: json.RawMessage(`{"content":"x"}`)}}},
		{deltas: []string{"抱歉，操作失败了。"}},
	}}
	exec := &recordingExec{err: errors.New("boom")}

	final, err := RunTurn(context.Background(), client, ChatRequest{}, exec.exec,
		func(string) error { return nil }, TurnOptions{})
	if err != nil {
		t.Fatalf("执行器错误不应中止循环: %v", err)
	}
	if final != "抱歉，操作失败了。" {
		t.Errorf("finalText = %q", final)
	}
	// 失败信息作为结果喂回模型
	second := client.popReq(1)
	toolMsg := second.Messages[len(second.Messages)-1]
	if toolMsg.Role != "tool" || !strings.Contains(toolMsg.Content, "工具执行失败: boom") {
		t.Errorf("工具失败应转成结果回喂: %+v", toolMsg)
	}
}

func TestRunTurnNoToolsSinglePass(t *testing.T) {
	client := newSingleRoundLLM([]string{"直接", "回答"}, nil)
	exec := &recordingExec{}

	final, err := RunTurn(context.Background(), client, ChatRequest{}, exec.exec,
		func(string) error { return nil }, TurnOptions{})
	if err != nil {
		t.Fatalf("RunTurn 失败: %v", err)
	}
	if final != "直接回答" {
		t.Errorf("finalText = %q", final)
	}
	if client.reqCount() != 1 {
		t.Errorf("无工具应只有一轮: %d", client.reqCount())
	}
	if exec.callCount() != 0 {
		t.Errorf("执行器不应被调用: %d", exec.callCount())
	}
}

func TestRunTurnRoundNotice(t *testing.T) {
	client := &fakeLLM{rounds: []fakeRound{
		{toolCalls: []ToolCall{{ID: "c1", Name: "send_markdown", Arguments: json.RawMessage(`{"content":"x"}`)}}},
		{deltas: []string{"好了。"}},
	}}
	exec := &recordingExec{result: `{"ok":true}`}

	var got []string
	final, err := RunTurn(context.Background(), client, ChatRequest{}, exec.exec,
		func(d string) error { got = append(got, d); return nil },
		TurnOptions{RoundNotice: "…"})
	if err != nil {
		t.Fatalf("RunTurn 失败: %v", err)
	}
	if final != "…好了。" {
		t.Errorf("finalText 应含轮间提示: %q", final)
	}
	if strings.Join(got, "") != final {
		t.Errorf("onDelta 序列应与 finalText 一致: %q", strings.Join(got, ""))
	}
}

func TestRunTurnFinalRoundOnly(t *testing.T) {
	// FinalRoundOnly：成功路径只返回最后一轮正文；出错仍回全程累积。
	client := &fakeLLM{rounds: []fakeRound{
		{
			deltas:    []string{"第一轮独白…"},
			toolCalls: []ToolCall{{ID: "c1", Name: "send_template_card", Arguments: json.RawMessage(`{"card":{"card_type":"text_notice"}}`)}},
		},
		{deltas: []string{"已发出，请查收。"}},
	}}
	exec := &recordingExec{result: `{"ok":true}`}

	var got []string
	final, err := RunTurn(context.Background(), client, ChatRequest{}, exec.exec,
		func(d string) error { got = append(got, d); return nil },
		TurnOptions{FinalRoundOnly: true})
	if err != nil {
		t.Fatalf("RunTurn 失败: %v", err)
	}
	if final != "已发出，请查收。" {
		t.Errorf("FinalRoundOnly 应只返回最后一轮正文: %q", final)
	}
	// 中间增量照常全程回调推送，不受净化影响
	if strings.Join(got, "") != "第一轮独白…已发出，请查收。" {
		t.Errorf("onDelta 应保持全程累积: %q", strings.Join(got, ""))
	}

	// 出错路径不作净化：半截全程文本随错误带回，由调用方闭环
	client2 := &fakeLLM{rounds: []fakeRound{
		{deltas: []string{"前半"}, roundErr: errAbort},
	}}
	final2, err := RunTurn(context.Background(), client2, ChatRequest{}, exec.exec,
		func(string) error { return nil }, TurnOptions{FinalRoundOnly: true})
	if final2 != "前半" {
		t.Errorf("出错时应回退全程累积文本: %q", final2)
	}
}

func TestRunTurnStreamErrorPropagates(t *testing.T) {
	client := &fakeLLM{rounds: []fakeRound{
		{deltas: []string{"半截"}, roundErr: errAbort},
	}}
	exec := &recordingExec{}

	final, err := RunTurn(context.Background(), client, ChatRequest{}, exec.exec,
		func(string) error { return nil }, TurnOptions{})
	if !errorsIs(err, errAbort) {
		t.Errorf("流错误应向上传播: %v", err)
	}
	if final != "半截" {
		t.Errorf("已累积文本应随返回值带回: %q", final)
	}
}
