package wecomaibot

import (
	"context"
	"time"

	"github.com/gorilla/websocket"
)

func (w *wsConnection) startHeartbeat(parent context.Context) {
	w.stopHeartbeat()
	// 新一轮心跳：清零未收到 ACK 的连续计数。
	w.missedPongCount.Store(0)

	// 绑定本轮心跳所属的连接：心跳 goroutine 只操作这个会话局部 conn，
	// 不再经 getConn() 取全局值——上一轮的残留 tick 在重连窗口期拿到
	// 旧 conn 做 Close 是幂等无害的；反过来残留 tick 若取到「新连接」
	// 的全局值去发帧/判断，则会污染新一轮心跳的计数与信道（stopHeartbeat
	// 只 cancel 不 join，残留 tick 是真实存在的竞态窗口）。
	conn := w.getConn()

	ctx, cancel := context.WithCancel(parent)
	w.heartbeatCancelMu.Lock()
	w.heartbeatCancel = cancel
	w.heartbeatCancelMu.Unlock()

	go func() {
		ticker := time.NewTicker(time.Duration(w.cfg.HeartbeatIntervalMS) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.sendHeartbeat(conn)
			}
		}
	}()
}

func (w *wsConnection) stopHeartbeat() {
	w.heartbeatCancelMu.Lock()
	defer w.heartbeatCancelMu.Unlock()
	if w.heartbeatCancel != nil {
		w.heartbeatCancel()
		w.heartbeatCancel = nil
	}
}

// maxMissedPong 是判定连接已死所需的「连续未收到心跳 ACK」次数（对齐官方 Node SDK）。
const maxMissedPong = 2

// sendHeartbeat 按周期发一个 ping 帧做保活（conn 是本轮心跳所属的
// 连接，见 startHeartbeat 注释），并以「连续 maxMissedPong 次未收到
// 心跳 ACK」作为死连接判据（对齐官方 Node SDK）：服务端会回 errcode=0
// 的 ACK（req_id 前缀 ping_），收到即清零计数；连续 2 次未收到则判定
// 连接已死，主动断开触发重连。
//
// 此外连接失效还能由写失败（ping 写不出去）或服务端被动断开来发现，
// 两者都会走重连；半开连接（TCP 已死但无 FIN/RST）仍由 TCP_USER_TIMEOUT
// 兜底。
func (w *wsConnection) sendHeartbeat(conn *websocket.Conn) {
	if w.missedPongCount.Load() >= maxMissedPong {
		w.logger.Warn("连续 %d 次心跳未收到 ACK，判定连接已死，主动断开以触发重连", maxMissedPong)
		if conn != nil {
			_ = conn.Close()
			// 若全局 conn 仍是这一个，同步置空让 IsConnected() 如实反映；
			// 若已被新一轮连接替换则不碰（新连接是别人的心跳轮次管）。
			if w.getConn() == conn {
				w.setConn(nil)
			}
		}
		return
	}
	w.missedPongCount.Add(1)

	frame := WsFrame{
		Cmd: WsCmdHeartbeat,
		Headers: WsHeaders{
			ReqID: GenerateReqID(WsCmdHeartbeat),
		},
	}
	if err := w.sendRawFrame(frame); err != nil {
		w.logger.Warn("发送心跳失败，主动断开以触发重连: %v", err)
		if conn != nil {
			_ = conn.Close()
			// 同上：只在自己仍是当前连接时置空全局值。
			if w.getConn() == conn {
				w.setConn(nil)
			}
		}
	}
}
