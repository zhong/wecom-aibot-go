package wecomaibot

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

type dispatcher struct {
	logger Logger

	mu sync.RWMutex

	onConnected     []func(context.Context)
	onAuthenticated []func(context.Context)
	onDisconnected  []func(context.Context, string)
	onReconnecting  []func(context.Context, int)
	onError         []func(context.Context, error)

	onMessage []func(context.Context, BaseMessage)
	onText    []func(context.Context, TextMessage)
	onImage   []func(context.Context, ImageMessage)
	onMixed   []func(context.Context, MixedMessage)
	onVoice   []func(context.Context, VoiceMessage)
	onFile    []func(context.Context, FileMessage)
	onVideo   []func(context.Context, VideoMessage)
	onEvent   []func(context.Context, EventMessage)

	onEnterChat         []func(context.Context, EventMessage)
	onTemplateCardEvent []func(context.Context, EventMessage)
	onFeedbackEvent     []func(context.Context, EventMessage)
	onDisconnectedEvent []func(context.Context, EventMessage)
}

func newDispatcher(logger Logger) *dispatcher {
	return &dispatcher{logger: logger}
}

func (d *dispatcher) addConnectedHandler(handler func(context.Context)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onConnected = append(d.onConnected, handler)
}

func (d *dispatcher) addAuthenticatedHandler(handler func(context.Context)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onAuthenticated = append(d.onAuthenticated, handler)
}

func (d *dispatcher) addDisconnectedHandler(handler func(context.Context, string)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onDisconnected = append(d.onDisconnected, handler)
}

func (d *dispatcher) addReconnectingHandler(handler func(context.Context, int)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onReconnecting = append(d.onReconnecting, handler)
}

func (d *dispatcher) addErrorHandler(handler func(context.Context, error)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onError = append(d.onError, handler)
}

func (d *dispatcher) addMessageHandler(handler func(context.Context, BaseMessage)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onMessage = append(d.onMessage, handler)
}

func (d *dispatcher) addTextHandler(handler func(context.Context, TextMessage)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onText = append(d.onText, handler)
}

func (d *dispatcher) addImageHandler(handler func(context.Context, ImageMessage)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onImage = append(d.onImage, handler)
}

func (d *dispatcher) addMixedHandler(handler func(context.Context, MixedMessage)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onMixed = append(d.onMixed, handler)
}

func (d *dispatcher) addVoiceHandler(handler func(context.Context, VoiceMessage)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onVoice = append(d.onVoice, handler)
}

func (d *dispatcher) addFileHandler(handler func(context.Context, FileMessage)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onFile = append(d.onFile, handler)
}

func (d *dispatcher) addVideoHandler(handler func(context.Context, VideoMessage)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onVideo = append(d.onVideo, handler)
}

func (d *dispatcher) addEventHandler(handler func(context.Context, EventMessage)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onEvent = append(d.onEvent, handler)
}

func (d *dispatcher) addEnterChatHandler(handler func(context.Context, EventMessage)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onEnterChat = append(d.onEnterChat, handler)
}

func (d *dispatcher) addTemplateCardEventHandler(handler func(context.Context, EventMessage)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onTemplateCardEvent = append(d.onTemplateCardEvent, handler)
}

func (d *dispatcher) addFeedbackEventHandler(handler func(context.Context, EventMessage)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onFeedbackEvent = append(d.onFeedbackEvent, handler)
}

func (d *dispatcher) addDisconnectedEventHandler(handler func(context.Context, EventMessage)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onDisconnectedEvent = append(d.onDisconnectedEvent, handler)
}

// snapshotHandlers 在读锁内复制一份回调切片：发射循环不再持锁，
// 新注册的 handler 对已开始的发射不可见（与既有行为一致）。
func snapshotHandlers[T any](d *dispatcher, handlers *[]T) []T {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return append([]T{}, *handlers...)
}

func (d *dispatcher) emitConnected(ctx context.Context) {
	for _, handler := range snapshotHandlers(d, &d.onConnected) {
		go handler(ctx)
	}
}

func (d *dispatcher) emitAuthenticated(ctx context.Context) {
	for _, handler := range snapshotHandlers(d, &d.onAuthenticated) {
		go handler(ctx)
	}
}

func (d *dispatcher) emitDisconnected(ctx context.Context, reason string) {
	for _, handler := range snapshotHandlers(d, &d.onDisconnected) {
		go handler(ctx, reason)
	}
}

func (d *dispatcher) emitReconnecting(ctx context.Context, attempt int) {
	for _, handler := range snapshotHandlers(d, &d.onReconnecting) {
		go handler(ctx, attempt)
	}
}

func (d *dispatcher) emitError(ctx context.Context, err error) {
	for _, handler := range snapshotHandlers(d, &d.onError) {
		go handler(ctx, err)
	}
}

func (d *dispatcher) dispatchFrame(ctx context.Context, frame WsFrameRaw) {
	switch frame.Cmd {
	case WsCmdCallback:
		d.dispatchMessage(ctx, frame)
	case WsCmdEventCallback:
		d.dispatchEvent(ctx, frame)
	default:
		d.logger.Debug("收到未分发 cmd: %s", frame.Cmd)
	}
}

func (d *dispatcher) dispatchMessage(ctx context.Context, frame WsFrameRaw) {
	var base BaseMessage
	if err := json.Unmarshal(frame.Body, &base); err != nil {
		d.emitError(ctx, fmt.Errorf("解析消息失败: %w", err))
		return
	}
	base.ReqID = frame.Headers.ReqID

	d.mu.RLock()
	genericHandlers := append([]func(context.Context, BaseMessage){}, d.onMessage...)
	textHandlers := append([]func(context.Context, TextMessage){}, d.onText...)
	imageHandlers := append([]func(context.Context, ImageMessage){}, d.onImage...)
	mixedHandlers := append([]func(context.Context, MixedMessage){}, d.onMixed...)
	voiceHandlers := append([]func(context.Context, VoiceMessage){}, d.onVoice...)
	fileHandlers := append([]func(context.Context, FileMessage){}, d.onFile...)
	videoHandlers := append([]func(context.Context, VideoMessage){}, d.onVideo...)
	d.mu.RUnlock()

	for _, handler := range genericHandlers {
		go handler(ctx, base)
	}

	switch base.MsgType {
	case MessageTypeText:
		var msg TextMessage
		if err := json.Unmarshal(frame.Body, &msg); err != nil {
			d.emitError(ctx, fmt.Errorf("解析文本消息失败: %w", err))
			return
		}
		msg.ReqID = frame.Headers.ReqID
		for _, handler := range textHandlers {
			go handler(ctx, msg)
		}
	case MessageTypeImage:
		var msg ImageMessage
		if err := json.Unmarshal(frame.Body, &msg); err != nil {
			d.emitError(ctx, fmt.Errorf("解析图片消息失败: %w", err))
			return
		}
		msg.ReqID = frame.Headers.ReqID
		for _, handler := range imageHandlers {
			go handler(ctx, msg)
		}
	case MessageTypeMixed:
		var msg MixedMessage
		if err := json.Unmarshal(frame.Body, &msg); err != nil {
			d.emitError(ctx, fmt.Errorf("解析图文混排消息失败: %w", err))
			return
		}
		msg.ReqID = frame.Headers.ReqID
		for _, handler := range mixedHandlers {
			go handler(ctx, msg)
		}
	case MessageTypeVoice:
		var msg VoiceMessage
		if err := json.Unmarshal(frame.Body, &msg); err != nil {
			d.emitError(ctx, fmt.Errorf("解析语音消息失败: %w", err))
			return
		}
		msg.ReqID = frame.Headers.ReqID
		for _, handler := range voiceHandlers {
			go handler(ctx, msg)
		}
	case MessageTypeFile:
		var msg FileMessage
		if err := json.Unmarshal(frame.Body, &msg); err != nil {
			d.emitError(ctx, fmt.Errorf("解析文件消息失败: %w", err))
			return
		}
		msg.ReqID = frame.Headers.ReqID
		for _, handler := range fileHandlers {
			go handler(ctx, msg)
		}
	case MessageTypeVideo:
		var msg VideoMessage
		if err := json.Unmarshal(frame.Body, &msg); err != nil {
			d.emitError(ctx, fmt.Errorf("解析视频消息失败: %w", err))
			return
		}
		msg.ReqID = frame.Headers.ReqID
		for _, handler := range videoHandlers {
			go handler(ctx, msg)
		}
	default:
		d.logger.Debug("收到未处理消息类型: %s", base.MsgType)
	}
}

func (d *dispatcher) dispatchEvent(ctx context.Context, frame WsFrameRaw) {
	var eventMsg EventMessage
	if err := json.Unmarshal(frame.Body, &eventMsg); err != nil {
		d.emitError(ctx, fmt.Errorf("解析事件失败: %w", err))
		return
	}
	eventMsg.ReqID = frame.Headers.ReqID

	d.mu.RLock()
	eventHandlers := append([]func(context.Context, EventMessage){}, d.onEvent...)
	enterHandlers := append([]func(context.Context, EventMessage){}, d.onEnterChat...)
	cardHandlers := append([]func(context.Context, EventMessage){}, d.onTemplateCardEvent...)
	feedbackHandlers := append([]func(context.Context, EventMessage){}, d.onFeedbackEvent...)
	disconnectedHandlers := append([]func(context.Context, EventMessage){}, d.onDisconnectedEvent...)
	d.mu.RUnlock()

	for _, handler := range eventHandlers {
		go handler(ctx, eventMsg)
	}

	switch eventMsg.Event.EventType {
	case EventTypeEnterChat:
		for _, handler := range enterHandlers {
			go handler(ctx, eventMsg)
		}
	case EventTypeTemplateCardEvent:
		for _, handler := range cardHandlers {
			go handler(ctx, eventMsg)
		}
	case EventTypeFeedbackEvent:
		for _, handler := range feedbackHandlers {
			go handler(ctx, eventMsg)
		}
	case EventTypeDisconnectedEvent:
		for _, handler := range disconnectedHandlers {
			go handler(ctx, eventMsg)
		}
	default:
		d.logger.Debug("收到未处理事件类型: %s", eventMsg.Event.EventType)
	}
}
