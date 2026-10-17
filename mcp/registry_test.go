package mcp

import (
	"sync"
	"testing"

	wecomaibot "github.com/zhong/wecom-aibot-go"
)

func newCarrier(chatID string) wecomaibot.TextMessage {
	m := wecomaibot.TextMessage{}
	m.ReqID = "req_" + chatID
	m.ChatID = chatID
	return m
}

func TestRegistrySetGetClear(t *testing.T) {
	r := newRegistry()
	if _, ok := r.get("chat_1"); ok {
		t.Fatal("空注册表不应命中")
	}
	r.set("chat_1", newCarrier("chat_1"))
	e, ok := r.get("chat_1")
	if !ok {
		t.Fatal("set 后应命中")
	}
	if e.req.RequestID() != "req_chat_1" {
		t.Errorf("快照载体不对: %v", e.req.RequestID())
	}
	// 同键覆盖
	r.set("chat_1", newCarrier("over"))
	if e, _ := r.get("chat_1"); e.req.RequestID() != "req_over" {
		t.Errorf("同键应覆盖: %v", e.req.RequestID())
	}
	r.clear("chat_1")
	if _, ok := r.get("chat_1"); ok {
		t.Error("clear 后不应命中")
	}
	// 清不存在的键无害
	r.clear("nope")
}

func TestRegistryConcurrent(t *testing.T) {
	// -race 下并发读写压注册表锁。
	r := newRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "chat_" + string(rune('a'+i))
			for j := 0; j < 200; j++ {
				r.set(key, newCarrier(key))
				r.get(key)
				r.get("other")
				if j%3 == 0 {
					r.clear(key)
				}
			}
		}(i)
	}
	wg.Wait()
}
