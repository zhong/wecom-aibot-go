package mcp

import (
	"sync"
	"testing"
	"time"
)

// TestSendLimiterAllowRelease 覆盖限速器核心语义：
// 首次放行、窗口内拒绝且不占额度、窗口外放行、release 回滚、
// 会话间互不影响、interval 0 恒放行。
func TestSendLimiterAllowRelease(t *testing.T) {
	l := newSendLimiter(200 * time.Millisecond)

	// 首次放行。
	if ok, _ := l.allow("chat_1"); !ok {
		t.Fatal("首次发送应放行")
	}
	// 窗口内第二次拒绝，且等待时长落在 (0, 200ms]。
	ok, wait := l.allow("chat_1")
	if ok || wait <= 0 || wait > 200*time.Millisecond {
		t.Fatalf("窗口内应拒绝: ok=%v wait=%v", ok, wait)
	}
	// 被限不占额度：release 后立刻可再发。
	ok, _ = l.allow("chat_1")
	if ok {
		t.Fatal("窗口内第二次应被拒")
	}
	l.release("chat_1")
	if ok, _ := l.allow("chat_1"); !ok {
		t.Fatal("release 后应放行（被拒的请求不该顺延窗口）")
	}

	// 会话隔离：chat_1 刚发过不影响 chat_2 首发。
	if ok, _ := l.allow("chat_2"); !ok {
		t.Fatal("不同会话应独立限速")
	}

	// 窗口过后放行。
	l2 := newSendLimiter(20 * time.Millisecond)
	if ok, _ := l2.allow("chat_1"); !ok {
		t.Fatal("首次发送应放行")
	}
	time.Sleep(25 * time.Millisecond)
	if ok, _ := l2.allow("chat_1"); !ok {
		t.Fatal("窗口过后应放行")
	}

	// interval 0（关闭）恒放行。
	l3 := newSendLimiter(0)
	for i := 0; i < 5; i++ {
		if ok, _ := l3.allow("chat_1"); !ok {
			t.Fatal("interval 0 应恒放行")
		}
	}

	// nil 限速器（未配置）恒放行、release 不 panic。
	var nilL *sendLimiter
	if ok, _ := nilL.allow("chat_1"); !ok {
		t.Fatal("nil 限速器应恒放行")
	}
	nilL.release("chat_1")

	// 负值按 0 处理（关闭）。
	l4 := newSendLimiter(-time.Second)
	if ok, _ := l4.allow("chat_1"); !ok {
		t.Fatal("负值应视为关闭")
	}
}

// TestSendLimiterConcurrent 并发 allow 无竞态（-race 下消证）。
func TestSendLimiterConcurrent(t *testing.T) {
	l := newSendLimiter(time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				ok, _ := l.allow("chat_1")
				if !ok {
					l.release("chat_1")
				}
			}
		}()
	}
	wg.Wait()
}
