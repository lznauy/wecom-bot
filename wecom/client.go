// Package wecom 实现企业微信"智能机器人"官方 WebSocket 协议客户端。
//
// 协议层适配自 NekoCode (interaction/connect/wecom/client.go)，在此基础上导出类型
// 并补充事件解析，供独立 daemon 使用。
package wecom

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	DefaultWSURL = "wss://openws.work.weixin.qq.com"
	cmdSubscribe = "aibot_subscribe"
	cmdHeartbeat = "ping"
	cmdCallback  = "aibot_msg_callback"
	cmdEvent     = "aibot_event_callback"
	cmdRespond   = "aibot_respond_msg"
	cmdSend      = "aibot_send_msg"
	// 分片上传临时素材：init → chunk × N → finish。
	cmdUploadInit   = "aibot_upload_media_init"
	cmdUploadChunk  = "aibot_upload_media_chunk"
	cmdUploadFinish = "aibot_upload_media_finish"

	maxTextBytes  = 20480
	chunkSize     = 512 * 1024 // 单分片 512KB（Base64 编码前）
	maxChunks     = 100        // 最多 100 片，即文件上限约 50MB
	writeTimeout  = 10 * time.Second
	uploadTimeout = 30 * time.Second // 单个上传请求（大分片）的 ack 等待
)

// ErrSuperseded 表示同一 BotID 的另一个连接接管了会话，重连无意义。
var ErrSuperseded = errors.New("wecom connection superseded by another client")

type frameHeaders struct {
	RequestID string `json:"req_id"`
}

type wsFrame struct {
	Command  string          `json:"cmd,omitempty"`
	Headers  frameHeaders    `json:"headers"`
	Body     json.RawMessage `json:"body,omitempty"`
	ErrCode  *int            `json:"errcode,omitempty"`
	ErrorMsg string          `json:"errmsg,omitempty"`
}

// Message 是一条来自企业微信的回调消息（文本/语音）或事件。
type Message struct {
	// ID 是 msgid，用作去重键。
	ID string
	// BotID 是机器人 ID。
	BotID string
	// ChatID 是群聊 ID；私聊为空。
	ChatID string
	// ChatType: "group" 或 "single"。
	ChatType string
	// FromUserID 是发送者企业微信 UserID。
	FromUserID string
	// MsgType: "text" / "voice" / ... 其他类型。
	MsgType string
	// Text 是文本内容（语音消息为转写内容）。
	Text string
	// EventType 非空表示这是一条事件而非聊天消息（如进群/退群通知）。
	EventType string
	// Raw 保留事件原始 body，供上层解析扩展字段。
	Raw json.RawMessage
}

// ChatTarget 返回消息归属会话的键：群聊为 ChatID，私聊为发送者 UserID。
func (m Message) ChatTarget() string {
	if m.ChatType == "group" && m.ChatID != "" {
		return m.ChatID
	}
	return m.FromUserID
}

// IsGroup 报告消息是否来自群聊。
func (m Message) IsGroup() bool { return m.ChatType == "group" && m.ChatID != "" }

// Client 维护一条已认证的企业微信 WebSocket 连接。
// 同一 BotID 仅允许一条连接；断线重连由调用方负责。
type Client struct {
	botID  string
	secret string
	wsURL  string

	mu    sync.Mutex
	conn  *websocket.Conn
	ready bool
	wait  map[string]chan wsFrame
}

// New 创建客户端。
func New(botID, secret string) *Client {
	return &Client{botID: botID, secret: secret, wsURL: DefaultWSURL, wait: make(map[string]chan wsFrame)}
}

// Serve 阻塞运行一次完整的 WebSocket 会话（认证 → 收发 → 连接结束）。
// onReady 在认证成功后回调一次；onMessage 在每条消息/事件到达时回调，
// reqID 是回调帧的 request id，用于被动回复（ReplyText）。
// 返回 ErrSuperseded 时表示被其他连接踢下线。
func (c *Client) Serve(ctx context.Context, onReady func(), onMessage func(ctx context.Context, reqID string, msg Message)) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.wsURL, nil)
	if err != nil {
		return err
	}
	c.setConn(conn)
	defer c.clearConn(conn)

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	authID := requestID(cmdSubscribe)
	if err := c.write(ctx, wsFrame{
		Command: cmdSubscribe,
		Headers: frameHeaders{RequestID: authID},
		Body:    mustJSON(map[string]string{"bot_id": c.botID, "secret": c.secret}),
	}); err != nil {
		return err
	}

	ready := make(chan struct{})
	go c.heartbeat(ctx, done, ready, conn)
	authenticated := false
	for {
		// 心跳 ack 也走普通帧；75s 读超时可以探测 half-open 连接。
		_ = conn.SetReadDeadline(time.Now().Add(75 * time.Second))
		var incoming wsFrame
		if err := conn.ReadJSON(&incoming); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		switch incoming.Command {
		case cmdCallback:
			if !authenticated {
				continue
			}
			if msg, ok := parseCallback(incoming.Body); ok {
				onMessage(ctx, incoming.Headers.RequestID, msg)
			}
		case cmdEvent:
			if msg, ok := parseEvent(incoming.Body); ok {
				onMessage(ctx, "", msg)
			}
			if disconnected(incoming.Body) {
				return ErrSuperseded
			}
		default:
			if incoming.Headers.RequestID == authID {
				if incoming.ErrCode == nil || *incoming.ErrCode != 0 {
					return fmt.Errorf("wecom authentication failed: %s (code %s)", incoming.ErrorMsg, errorCode(incoming.ErrCode))
				}
				if !authenticated {
					authenticated = true
					c.markReady(conn)
					close(ready)
					if onReady != nil {
						onReady()
					}
				}
				continue
			}
			c.deliverAck(incoming)
		}
	}
}

func parseCallback(body json.RawMessage) (Message, bool) {
	var raw struct {
		MessageID string `json:"msgid"`
		BotID     string `json:"aibotid"`
		ChatID    string `json:"chatid,omitempty"`
		ChatType  string `json:"chattype"`
		From      struct {
			UserID string `json:"userid"`
		} `json:"from"`
		MessageType string `json:"msgtype"`
		Text        struct {
			Content string `json:"content"`
		} `json:"text,omitempty"`
		Voice struct {
			Content string `json:"content"`
		} `json:"voice,omitempty"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Message{}, false
	}
	msg := Message{
		ID:         raw.MessageID,
		BotID:      raw.BotID,
		ChatID:     raw.ChatID,
		ChatType:   raw.ChatType,
		FromUserID: raw.From.UserID,
		MsgType:    raw.MessageType,
		Text:       raw.Text.Content,
		Raw:        body,
	}
	if raw.MessageType == "voice" {
		msg.Text = raw.Voice.Content
	}
	return msg, true
}

func parseEvent(body json.RawMessage) (Message, bool) {
	var raw struct {
		Event struct {
			EventType string `json:"eventtype"`
		} `json:"event"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Message{}, false
	}
	return Message{EventType: raw.Event.EventType, Raw: body}, true
}

func disconnected(body json.RawMessage) bool {
	var raw struct {
		Event struct {
			EventType string `json:"eventtype"`
		} `json:"event"`
	}
	if json.Unmarshal(body, &raw) != nil {
		return false
	}
	return raw.Event.EventType == "disconnected_event"
}

func (c *Client) heartbeat(ctx context.Context, done, ready <-chan struct{}, conn *websocket.Conn) {
	select {
	case <-ctx.Done():
		return
	case <-done:
		return
	case <-ready:
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			if err := c.write(ctx, wsFrame{Command: cmdHeartbeat, Headers: frameHeaders{RequestID: requestID(cmdHeartbeat)}}); err != nil {
				_ = conn.Close()
				return
			}
		}
	}
}

// ReplyText 通过被动回复接口应答一条回调消息（reqID 为该回调帧的 request id）。
func (c *Client) ReplyText(ctx context.Context, reqID, text string) error {
	body := map[string]any{
		"msgtype": "stream",
		"stream": map[string]any{
			"id":      requestID("stream"),
			"finish":  true,
			"content": truncateUTF8(text, maxTextBytes),
		},
	}
	return c.write(ctx, wsFrame{Command: cmdRespond, Headers: frameHeaders{RequestID: reqID}, Body: mustJSON(body)})
}

// SendMarkdown 主动向指定会话（群 ChatID 或用户 UserID）发送 markdown 消息，等待 ack。
func (c *Client) SendMarkdown(ctx context.Context, chatID, text string) error {
	return c.sendMessage(ctx, chatID, map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]string{"content": truncateUTF8(text, maxTextBytes)},
	})
}

// SendFile 主动向指定会话发送文件消息。mediaID 由 UploadMedia 获得。
func (c *Client) SendFile(ctx context.Context, chatID, mediaID string) error {
	return c.sendMessage(ctx, chatID, map[string]any{
		"msgtype": "file",
		"file":    map[string]string{"media_id": mediaID},
	})
}

func (c *Client) sendMessage(ctx context.Context, chatID string, body map[string]any) error {
	body["chatid"] = chatID
	reqID := requestID(cmdSend)
	ack := make(chan wsFrame, 1)
	if err := c.writeRequest(ctx, wsFrame{Command: cmdSend, Headers: frameHeaders{RequestID: reqID}, Body: mustJSON(body)}, ack); err != nil {
		return err
	}
	timer := time.NewTimer(writeTimeout)
	defer timer.Stop()
	defer c.removeWaiter(reqID)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.New("wecom send acknowledgement timed out")
	case response := <-ack:
		if response.Headers.RequestID == "" {
			// 连接关闭时会 close 等待通道，收到零值帧视为发送失败。
			return errors.New("wecom connection closed while awaiting acknowledgement")
		}
		if response.ErrCode == nil || *response.ErrCode != 0 {
			return fmt.Errorf("wecom send rejected: %s (code %s)", response.ErrorMsg, errorCode(response.ErrCode))
		}
		return nil
	}
}

// UploadMedia 通过 WS 通道分片上传临时素材，返回 media_id。
// mediaType 支持 file / image / voice / video；文件上限约 50MB（100 × 512KB）。
func (c *Client) UploadMedia(ctx context.Context, mediaType, filename string, data []byte) (string, error) {
	total := len(data)
	chunks := (total + chunkSize - 1) / chunkSize
	if chunks == 0 {
		chunks = 1
	}
	if chunks > maxChunks {
		return "", fmt.Errorf("file too large: %d bytes exceeds %d chunks limit (~%dMB)", total, maxChunks, maxChunks*chunkSize>>20)
	}
	sum := md5.Sum(data)

	initBody, err := c.request(ctx, cmdUploadInit, map[string]any{
		"type":         mediaType,
		"filename":     filename,
		"total_size":   total,
		"total_chunks": chunks,
		"md5":          hex.EncodeToString(sum[:]),
	}, writeTimeout)
	if err != nil {
		return "", fmt.Errorf("upload init: %w", err)
	}
	var initResp struct {
		UploadID string `json:"upload_id"`
	}
	if json.Unmarshal(initBody, &initResp) != nil || initResp.UploadID == "" {
		return "", fmt.Errorf("upload init: no upload_id in response")
	}

	for i := 0; i < chunks; i++ {
		start, end := i*chunkSize, (i+1)*chunkSize
		if end > total {
			end = total
		}
		_, err := c.request(ctx, cmdUploadChunk, map[string]any{
			"upload_id":   initResp.UploadID,
			"chunk_index": i,
			"base64_data": base64.StdEncoding.EncodeToString(data[start:end]),
		}, uploadTimeout)
		if err != nil {
			return "", fmt.Errorf("upload chunk %d/%d: %w", i+1, chunks, err)
		}
	}

	finishBody, err := c.request(ctx, cmdUploadFinish, map[string]any{
		"upload_id": initResp.UploadID,
	}, uploadTimeout)
	if err != nil {
		return "", fmt.Errorf("upload finish: %w", err)
	}
	var finishResp struct {
		MediaID string `json:"media_id"`
	}
	if json.Unmarshal(finishBody, &finishResp) != nil || finishResp.MediaID == "" {
		return "", fmt.Errorf("upload finish: no media_id in response")
	}
	return finishResp.MediaID, nil
}

// request 发送一帧并等待同 req_id 的 ack，返回 ack 的 body。
// timeout 同时约束写入与 ack 等待（大分片写入可能较慢，不能只依赖 writeTimeout）。
func (c *Client) request(ctx context.Context, cmd string, body any, timeout time.Duration) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	reqID := requestID(cmd)
	ack := make(chan wsFrame, 1)
	if err := c.writeRequest(ctx, wsFrame{Command: cmd, Headers: frameHeaders{RequestID: reqID}, Body: mustJSON(body)}, ack); err != nil {
		return nil, err
	}
	defer c.removeWaiter(reqID)
	select {
	case <-ctx.Done():
		if err := ctx.Err(); err != context.DeadlineExceeded {
			return nil, err
		}
		return nil, errors.New("wecom request timed out")
	case response, ok := <-ack:
		if !ok || response.Headers.RequestID == "" {
			return nil, errors.New("wecom connection closed while awaiting acknowledgement")
		}
		if response.ErrCode == nil || *response.ErrCode != 0 {
			return nil, fmt.Errorf("wecom %s rejected: %s (code %s)", cmd, response.ErrorMsg, errorCode(response.ErrCode))
		}
		return response.Body, nil
	}
}

func (c *Client) write(ctx context.Context, frame wsFrame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.conn == nil {
		return errors.New("wecom websocket is not connected")
	}
	deadline := time.Now().Add(writeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return c.conn.WriteJSON(frame)
}

func (c *Client) setConn(conn *websocket.Conn) {
	c.mu.Lock()
	c.conn = conn
	c.ready = false
	c.mu.Unlock()
}

func (c *Client) markReady(conn *websocket.Conn) {
	c.mu.Lock()
	if c.conn == conn {
		c.ready = true
	}
	c.mu.Unlock()
}

func (c *Client) clearConn(conn *websocket.Conn) {
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
		c.ready = false
	}
	for reqID, waiter := range c.wait {
		delete(c.wait, reqID)
		close(waiter)
	}
	c.mu.Unlock()
	_ = conn.Close()
}

func (c *Client) writeRequest(ctx context.Context, frame wsFrame, ack chan wsFrame) error {
	c.mu.Lock()
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return err
	}
	if c.conn == nil || !c.ready {
		c.mu.Unlock()
		return errors.New("wecom websocket is not authenticated")
	}
	c.wait[frame.Headers.RequestID] = ack
	deadline := time.Now().Add(writeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		delete(c.wait, frame.Headers.RequestID)
		c.mu.Unlock()
		return err
	}
	err := c.conn.WriteJSON(frame)
	if err != nil {
		delete(c.wait, frame.Headers.RequestID)
	}
	c.mu.Unlock()
	return err
}

func (c *Client) deliverAck(frame wsFrame) {
	c.mu.Lock()
	waiter := c.wait[frame.Headers.RequestID]
	if waiter != nil {
		delete(c.wait, frame.Headers.RequestID)
		waiter <- frame
	}
	c.mu.Unlock()
}

func (c *Client) removeWaiter(reqID string) {
	c.mu.Lock()
	delete(c.wait, reqID)
	c.mu.Unlock()
}

func requestID(prefix string) string { return prefix + "_" + uuid.NewString() }

func mustJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

func errorCode(code *int) string {
	if code == nil {
		return "missing"
	}
	return fmt.Sprint(*code)
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
