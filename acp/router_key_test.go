package acp

import (
	"testing"

	wecomaibot "github.com/zhong/wecom-aibot-go"
)

// TestChatKeyOfReqAllCarriers 参数化钉死路由键提取语义：六种消息载体上，
// 群聊（ChatID 非空）以 ChatID 为键，单聊（ChatID 空）回退 dm:<userID>；
// 未知载体返回空串。
func TestChatKeyOfReqAllCarriers(t *testing.T) {
	tests := []struct {
		name string
		req  wecomaibot.RequestCarrier
		want string
	}{
		{"文本-群聊", wecomaibot.TextMessage{BaseMessage: wecomaibot.BaseMessage{ChatID: "wr_Group", MsgID: "m1"}}, "wr_Group"},
		{"文本-单聊回退dm", wecomaibot.TextMessage{BaseMessage: wecomaibot.BaseMessage{MsgID: "m2", From: wecomaibot.MessageFrom{UserID: "alice"}}}, "dm:alice"},
		{"图片-群聊", wecomaibot.ImageMessage{BaseMessage: wecomaibot.BaseMessage{ChatID: "wr_G2", MsgID: "m3"}}, "wr_G2"},
		{"图片-单聊", wecomaibot.ImageMessage{BaseMessage: wecomaibot.BaseMessage{MsgID: "m4", From: wecomaibot.MessageFrom{UserID: "bob"}}}, "dm:bob"},
		{"语音-群聊", wecomaibot.VoiceMessage{BaseMessage: wecomaibot.BaseMessage{ChatID: "wr_G3", MsgID: "m5"}}, "wr_G3"},
		{"语音-单聊", wecomaibot.VoiceMessage{BaseMessage: wecomaibot.BaseMessage{MsgID: "m6", From: wecomaibot.MessageFrom{UserID: "carol"}}}, "dm:carol"},
		{"视频-群聊", wecomaibot.VideoMessage{BaseMessage: wecomaibot.BaseMessage{ChatID: "wr_G4", MsgID: "m7"}}, "wr_G4"},
		{"视频-单聊", wecomaibot.VideoMessage{BaseMessage: wecomaibot.BaseMessage{MsgID: "m8", From: wecomaibot.MessageFrom{UserID: "dave"}}}, "dm:dave"},
		{"文件-群聊", wecomaibot.FileMessage{BaseMessage: wecomaibot.BaseMessage{ChatID: "wr_G5", MsgID: "m9"}}, "wr_G5"},
		{"文件-单聊", wecomaibot.FileMessage{BaseMessage: wecomaibot.BaseMessage{MsgID: "m10", From: wecomaibot.MessageFrom{UserID: "erin"}}}, "dm:erin"},
		{"混排-群聊", wecomaibot.MixedMessage{BaseMessage: wecomaibot.BaseMessage{ChatID: "wr_G6", MsgID: "m11"}}, "wr_G6"},
		{"混排-单聊", wecomaibot.MixedMessage{BaseMessage: wecomaibot.BaseMessage{MsgID: "m12", From: wecomaibot.MessageFrom{UserID: "frank"}}}, "dm:frank"},
		// 事件不是路由输入（EventMessage 分支已随 initializeSession 透传层
		// 一并删除），落 default 返回空串，路由层据此拒绝。
		{"事件-不受路由支持", wecomaibot.EventMessage{ChatID: "wr_G7", MsgID: "m13"}, ""},
		{"nil-未知载体", nil, ""},
	}
	for _, tt := range tests {
		if got := chatKeyOfReq(tt.req); got != tt.want {
			t.Errorf("%s: chatKeyOfReq = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// TestExtractChatIDAllCarriers 参数化钉死终帧降级目标提取：七种载体上
// ChatID 原样透传（含事件），空 ChatID 与未知载体返回空串表示无法降级。
func TestExtractChatIDAllCarriers(t *testing.T) {
	tests := []struct {
		name string
		req  wecomaibot.RequestCarrier
		want string
	}{
		{"文本-群聊", wecomaibot.TextMessage{BaseMessage: wecomaibot.BaseMessage{ChatID: "wr_G1"}}, "wr_G1"},
		{"文本-单聊无ChatID", wecomaibot.TextMessage{}, ""},
		{"图片", wecomaibot.ImageMessage{BaseMessage: wecomaibot.BaseMessage{ChatID: "wr_G2"}}, "wr_G2"},
		{"语音", wecomaibot.VoiceMessage{BaseMessage: wecomaibot.BaseMessage{ChatID: "wr_G3"}}, "wr_G3"},
		{"视频", wecomaibot.VideoMessage{BaseMessage: wecomaibot.BaseMessage{ChatID: "wr_G4"}}, "wr_G4"},
		{"文件", wecomaibot.FileMessage{BaseMessage: wecomaibot.BaseMessage{ChatID: "wr_G5"}}, "wr_G5"},
		{"混排", wecomaibot.MixedMessage{BaseMessage: wecomaibot.BaseMessage{ChatID: "wr_G6"}}, "wr_G6"},
		{"事件-也透传", wecomaibot.EventMessage{ChatID: "wr_G7"}, "wr_G7"},
		{"事件-单聊无ChatID", wecomaibot.EventMessage{}, ""},
		{"nil-未知载体", nil, ""},
	}
	for _, tt := range tests {
		if got := extractChatID(tt.req); got != tt.want {
			t.Errorf("%s: extractChatID = %q, want %q", tt.name, got, tt.want)
		}
	}
}
