package mcp

import (
	"sync"
	"time"
)

// DefaultSendMinInterval 是外发类工具（send_template_card / send_markdown）
// 两次成功发送之间的默认最小间隔。注入滥用最坏形态是一轮 prompt 内连发
// 洪水消息刷屏群聊——每会话限速把伤害有界化，正常模型交互（一轮最多
// 一两张卡片/一条 markdown）远低于该阈值，不会感知。
const DefaultSendMinInterval = 3 * time.Second

// sendTools 是受限速约束的外发类工具名集合（read_attachment 只读不
// 触发企微消息面，不限）。
var sendTools = map[string]bool{
	"send_template_card": true,
	"send_markdown":      true,
}

// sendLimiter 是按会话的外发间隔限速器：同一会话的相邻两次成功发送之间
// 必须间隔 minInterval（滑动窗口只保留最后一次时间戳，即最小间隔语义）。
// 首次发送总是放行。被限的调用返回 false 与建议等待时长——调用方把这
// 个信息作为正常工具结果回给模型，模型可自纠（等待或放弃），不当协议错。
type sendLimiter struct {
	minInterval time.Duration

	mu     sync.Mutex
	lastBy map[string]time.Time
}

func newSendLimiter(minInterval time.Duration) *sendLimiter {
	if minInterval < 0 {
		minInterval = 0
	}
	return &sendLimiter{minInterval: minInterval, lastBy: map[string]time.Time{}}
}

// allow 返回该会话此刻是否可发送；不可时 relayIn 是距下次可发送的时长。
// minInterval 为 0 的限速器总是放行（等价于未启用）。
func (l *sendLimiter) allow(chatKey string) (ok bool, relayIn time.Duration) {
	if l == nil || l.minInterval == 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if last, ok := l.lastBy[chatKey]; ok {
		if elapsed := now.Sub(last); elapsed < l.minInterval {
			return false, l.minInterval - elapsed
		}
	}
	l.lastBy[chatKey] = now
	return true, 0
}

// release 回滚一次 allow 的占用：发送未真正成功时不消耗该会话的额度，
// 失败重试不会被自己挡住。仅撤销最近一次 allow 登记的时间戳。
func (l *sendLimiter) release(chatKey string) {
	if l == nil || l.minInterval == 0 {
		return
	}
	l.mu.Lock()
	delete(l.lastBy, chatKey)
	l.mu.Unlock()
}

// SetSendMinInterval 设定外发类工具的每会话最小发送间隔（频控）。
// 非 <=0 的值都会启用限速（0 = 关闭，DefaultSendMinInterval 为默认值）。
// 沙箱化 agent 部署必配：注入可让 agent 滥用发送面刷屏群聊，沙箱管
// 不住这一面（见 docs/sandbox-integration-sketch.md 残余风险第 3 条），
// 连接层限速把伤害有界化。必须在 Start 前调用。
func (s *WeComServer) SetSendMinInterval(d time.Duration) {
	s.sendLimiter = newSendLimiter(d)
}
