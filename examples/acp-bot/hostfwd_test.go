package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// TestParseForward 覆盖三种形态与非法输入。
func TestParseForward(t *testing.T) {
	cases := []struct {
		in        string
		wantPort  int
		wantTgt   string
		wantError bool
	}{
		{"3425:3425", 3425, "127.0.0.1:3425", false},
		{"3425:8443", 3425, "127.0.0.1:8443", false},
		{"8080:127.0.0.1:80", 8080, "127.0.0.1:80", false},
		{"9:9", 9, "127.0.0.1:9", false}, // 低端口合法（仅 0/负数拒绝）
		{"a:b", 0, "", true},             // 非数字
		{"3425", 0, "", true},            // 缺目标
		{"3425:", 0, "", true},           // 空目标
		{"1:2:3:4", 0, "", true},         // 段数超标
	}
	for _, c := range cases {
		port, tgt, err := parseForward(c.in)
		if c.wantError {
			if err == nil {
				t.Errorf("parseForward(%q) 应报错", c.in)
			}
			continue
		}
		if err != nil || port != c.wantPort || tgt != c.wantTgt {
			t.Errorf("parseForward(%q) = (%d, %q, %v), 期望 (%d, %q)", c.in, port, tgt, err, c.wantPort, c.wantTgt)
		}
	}
}

// TestStartHostForwardsE2E 起真实回环 HTTP 服务，经转发器访问，验证
// 数据双向透传与 closer 全量关闭。
func TestStartHostForwardsE2E(t *testing.T) {
	// 回环目标：真实 httptest 服务端
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("loopback-up"))
	}))
	t.Cleanup(target.Close)

	// 绑定地址避开 192.168.64.1（测试机可能没有该网桥）：用回环本身
	// 验证转发逻辑；bind host 由 ACP_HOSTFWD_BIND 控制。
	t.Setenv("ACP_HOSTFWD_BIND", "127.0.0.1")
	// 目标端口从 httptest URL 提取
	_, tgtPort, _ := net.SplitHostPort(target.Listener.Addr().String())
	spec := "18443:" + tgtPort // 对外端口 != 目标端口，验证两侧各自取值

	closer, err := startHostForwards(spec)
	if err != nil {
		t.Fatalf("startHostForwards: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	resp, err := http.Get("http://127.0.0.1:18443/ping")
	if err != nil {
		t.Fatalf("经转发器访问失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "loopback-up" {
		t.Errorf("经转发器正文 = %q, 期望 loopback-up", body)
	}

	// closer 关闭后端口应不可再连
	if err := closer.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:18443", 200*time.Millisecond)
		if err != nil {
			break // 已关
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Error("关闭后端口仍可连")
			break
		}
	}
}

// TestStartHostForwardsMultiAndFail 一条失败整体失败（不留半开暴露面），
// 多条并存全通。
func TestStartHostForwardsMultiAndFail(t *testing.T) {
	t.Setenv("ACP_HOSTFWD_BIND", "127.0.0.1")

	// 非法 spec 整体失败
	if _, err := startHostForwards("18444:18443,bad"); err == nil {
		t.Error("含非法条目应整体失败")
	}

	// 端口冲突闭环：先占一个端口再转发同端口，应失败且不留监听
	ln, err := net.Listen("tcp", "127.0.0.1:18445")
	if err != nil {
		t.Skipf("18445 被占用，跳过: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	if _, err := startHostForwards("18445:18443"); err == nil {
		t.Error("端口被占应失败")
	}

	// 多条并存全通（各自独立端口）
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(target.Close)
	_, tgtPort, _ := net.SplitHostPort(target.Listener.Addr().String())
	closer, err := startHostForwards("18446:" + tgtPort + ",18447:" + tgtPort)
	if err != nil {
		t.Fatalf("startHostForwards 多条: %v", err)
	}
	defer func() { _ = closer.Close() }()
	for _, p := range []string{"18446", "18447"} {
		resp, err := http.Get("http://127.0.0.1:" + p + "/")
		if err != nil {
			t.Errorf("经 %s 访问失败: %v", p, err)
			continue
		}
		_ = resp.Body.Close()
	}
}

// TestStartHostForwardsEmpty 空 spec 是 no-op（不绑任何端口）。
func TestStartHostForwardsEmpty(t *testing.T) {
	_ = os.Unsetenv("ACP_HOSTFWD_BIND")
	c, err := startHostForwards("")
	if err != nil {
		t.Fatalf("空 spec: %v", err)
	}
	if c != nil {
		_ = c.Close()
	}
}
