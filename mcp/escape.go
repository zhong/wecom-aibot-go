package mcp

import "net/url"

// chatKeyEscape 把 chatKey 编码为 URL 路径段（dm: 单聊键含冒号——冒号
// 是合法路径段字符，PathEscape 不转义它）。PathEscape 对路径段的编码
// 与文件名安全需求一致（斜杠等会转义）。
func chatKeyEscape(chatKey string) string {
	return url.PathEscape(chatKey)
}

// chatKeyUnescape 还原 chatKey；解码失败按未知端点处理。
// 注意入参须是「未解码」的路径段——net/http 的 r.URL.Path 已经解码过
// 一次，再对其解码会双重失真（含 % 的键与转义后的别的键坍缩到同一
// 端点），调用方应传 r.URL.EscapedPath() 的对应段。
func chatKeyUnescape(seg string) (string, error) {
	return url.PathUnescape(seg)
}
