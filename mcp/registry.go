package mcp

import (
	"sync"

	wecomaibot "github.com/zhong/wecom-aibot-go"
)

// chatEntry 是一个在途消息的快照：req 是触发本轮 prompt 的消息载体
// （流式回复 req_id 与附件上下文的来源），saveDir 是该会话 agent 的
// cwd（二进制附件落盘目录，可为空）。
type chatEntry struct {
	req     wecomaibot.RequestCarrier
	saveDir string
}

// registry 维护 chatKey → 在途消息快照。SetChat 在一条消息的 prompt
// 在途期间登记，ClearChat 在收尾后清除；acp.Router 的每会话串行锁保证
// 同一 chatKey 的快照不会被下一条消息覆盖读取。
type registry struct {
	mu sync.RWMutex
	m  map[string]chatEntry
}

func newRegistry() *registry {
	return &registry{m: map[string]chatEntry{}}
}

// set 登记快照；同 chatKey 覆盖。
func (r *registry) set(chatKey string, req wecomaibot.RequestCarrier, saveDir string) {
	r.mu.Lock()
	r.m[chatKey] = chatEntry{req: req, saveDir: saveDir}
	r.mu.Unlock()
}

// clear 清除快照。
func (r *registry) clear(chatKey string) {
	r.mu.Lock()
	delete(r.m, chatKey)
	r.mu.Unlock()
}

// get 取快照；不存在返回 false。
func (r *registry) get(chatKey string) (chatEntry, bool) {
	r.mu.RLock()
	e, ok := r.m[chatKey]
	r.mu.RUnlock()
	return e, ok
}
