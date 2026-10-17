package mcp

import "net/url"

// chatKeyEscape 把 chatKey 编码为 URL 路径段（dm: 单聊键含冒号）。
// PathEscape 对路径段的编码与文件名安全需求一致（斜杠、冒号等都会转义）。
func chatKeyEscape(chatKey string) string {
	return url.PathEscape(chatKey)
}

// chatKeyUnescape 还原 chatKey；解码失败按未知端点处理。
func chatKeyUnescape(seg string) (string, error) {
	return url.PathUnescape(seg)
}
