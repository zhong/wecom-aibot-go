// hostfwd.go 是沙箱 agent 部署的宿主回环转发器（Apple container 路线的
// 归宿 B）：容器经 vmnet 网关（如 192.168.64.1）访问宿主，但外部服务
// （LLM 代理等）通常只监听 127.0.0.1。ACP_HOSTFWD 让本示例在自身进程内
// 把「网关 IP:端口」逐端口转发到「127.0.0.1:回环端口」——外部服务零改动，
// 转发随 bot 进程存活（没有会悄悄死掉的独立转发进程）。
//
// docker/OrbStack 路线不需要它：host.docker.internal 引擎级桥接全部
// 回环端口。
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
)

// startHostForwards 解析 ACP_HOSTFWD 并启动全部转发器。
//
// 格式（逗号分隔多条，冒号分两侧）：
//
//	ACP_HOSTFWD=3425:3425,8080:127.0.0.1:80
//
// 每条是「对外端口:回环目标」，回环目标缺省 host 视为同端口（即
// "<port>:<port>" 直接省写目标端口）；对外地址固定取 ACP_HOSTFWD_BIND
// （缺省容器网关 192.168.64.1——Apple container 容器网络的宿主网桥 IP）。
// 转发器全由返回的 closer 统一关闭；任何一条监听/格式失败即整体失败
// （半开的暴露面比报错退出更糟）。
func startHostForwards(spec string) (io.Closer, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	bindHost := os.Getenv("ACP_HOSTFWD_BIND")
	if bindHost == "" {
		bindHost = "192.168.64.1"
	}
	var closers []io.Closer
	for _, fwd := range strings.Split(spec, ",") {
		outPort, target, err := parseForward(fwd)
		if err != nil {
			for _, c := range closers {
				_ = c.Close()
			}
			return nil, err
		}
		c, err := forwardOne(bindHost, outPort, target)
		if err != nil {
			for _, c := range closers {
				_ = c.Close()
			}
			return nil, err
		}
		closers = append(closers, c)
	}
	return joinClosers(closers), nil
}

// parseForward 解析单条 "对外端口:回环目标"。目标可以是端口（补
// 127.0.0.1 前缀）或 host:port 全形。
func parseForward(fwd string) (int, string, error) {
	parts := strings.Split(strings.TrimSpace(fwd), ":")
	if len(parts) == 2 && parts[1] != "" {
		outPort, err := strconv.Atoi(parts[0])
		if err != nil || outPort <= 0 {
			return 0, "", fmt.Errorf("ACP_HOSTFWD 端口无效: %q", fwd)
		}
		if _, err := strconv.Atoi(parts[1]); err == nil {
			return outPort, "127.0.0.1:" + parts[1], nil
		}
		return outPort, parts[1], nil
	}
	if len(parts) == 3 {
		outPort, err := strconv.Atoi(parts[0])
		if err != nil || outPort <= 0 {
			return 0, "", fmt.Errorf("ACP_HOSTFWD 端口无效: %q", fwd)
		}
		return outPort, parts[1] + ":" + parts[2], nil
	}
	return 0, "", fmt.Errorf("ACP_HOSTFWD 格式无效: %q（应为 端口:目标 或 端口:host:port）", fwd)
}

// forwardOne 在 bindHost:outPort 监听并把每条连接原样转发到 target。
func forwardOne(bindHost string, outPort int, target string) (io.Closer, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(bindHost, strconv.Itoa(outPort)))
	if err != nil {
		return nil, fmt.Errorf("宿主转发监听 %s:%d 失败: %w", bindHost, outPort, err)
	}
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return // closer 已关/监听损坏：转发随 bot 生命周期结束
			}
			go func(in net.Conn) {
				defer in.Close()
				out, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer out.Close()
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(out, in); done <- struct{}{} }()
				go func() { _, _ = io.Copy(in, out); done <- struct{}{} }()
				<-done
			}(in)
		}
	}()
	return ln, nil
}

// joinClosers 把多个 closer 收拢成一个；任一失败不影响其余关闭。
func joinClosers(cs []io.Closer) io.Closer {
	return closerList(cs)
}

type closerList []io.Closer

func (c closerList) Close() error {
	var firstErr error
	for _, x := range c {
		if err := x.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
