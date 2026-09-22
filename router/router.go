// Package router 将企业微信消息按会话目标（群 ChatID / 私聊 UserID）
// 路由到独立的 session，并负责会话生命周期与消息去重。
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wecom-bot/session"
	"wecom-bot/wecom"
)

const (
	seenTTL        = 10 * time.Minute
	seenMax        = 4096
	sessionMapFile = ".wecom-sessions.json" // chatID → session_id 映射
	// 恢复会话的子进程在启动后短时间内即异常退出，视为 resume 失败，
	// 清除映射以便下次新建会话。
	resumeFailWindow = 15 * time.Second
)

// Options 路由器配置。
type Options struct {
	Session session.Options
	// WorkDirBase 是每群独立工作目录的父目录；为空则全部共用进程 cwd。
	WorkDirBase string
	// SharedWorkDir 非空时，所有会话共用该已存在的工作区（优先于 WorkDirBase）。
	SharedWorkDir string
}

// Router 维护 chatID → *session.Session 映射。
type Router struct {
	opts   Options
	sender *wecom.Client

	mu       sync.Mutex
	sessions map[string]*entry

	persistMu sync.Mutex
	seenMu    sync.Mutex
	seen      map[string]time.Time
}

type entry struct {
	sess     *session.Session
	chatID   string
	lastUsed time.Time
}

// New 创建路由器。sender 用于向群聊投递会话输出。
func New(opts Options, sender *wecom.Client) *Router {
	return &Router{opts: opts, sender: sender, sessions: make(map[string]*entry), seen: make(map[string]time.Time)}
}

// Dispatch 处理一条入站消息。
func (r *Router) Dispatch(ctx context.Context, sender *wecom.Client, reqID string, msg wecom.Message) {
	if msg.EventType != "" {
		// 进群/退群等事件：目前仅记录日志，session 仍按消息驱动创建。
		log.Printf("[router] event: %s raw=%s", msg.EventType, msg.Raw)
		return
	}
	if msg.ID != "" && !r.accept(msg.BotID+"\x00"+msg.ID) {
		return // 重复投递，静默丢弃
	}
	chatID := msg.ChatTarget()
	if chatID == "" || msg.Text == "" {
		return
	}
	// 群聊文本带 @机器人 前缀（如 "@Bot 你好"），剥离后再做命令匹配 / 交给模型。
	text := msg.Text
	if msg.IsGroup() && strings.HasPrefix(text, "@") {
		body := strings.TrimPrefix(text, "@")
		if i := strings.IndexAny(body, " \t\n"); i >= 0 {
			text = strings.TrimLeft(body[i+1:], " \t")
		} else {
			text = "" // 只有 @机器人 没有正文
		}
	}
	if text == "" {
		return
	}
	preview := text
	if len(preview) > 50 {
		preview = preview[:50] + "…"
	}
	log.Printf("[router] message: chat=%s from=%s text=%q", chatID, msg.FromUserID, preview)
	if arg, ok := parseFileCommand(text); ok {
		r.sendFile(ctx, sender, reqID, chatID, arg)
		return
	}
	sess, err := r.sessionFor(ctx, chatID)
	if err != nil {
		log.Printf("[router] session for %s: %v", chatID, err)
		_ = sender.ReplyText(ctx, reqID, "会话创建失败: "+err.Error())
		return
	}
	if err := sess.Send(msg.ID, text); err != nil {
		log.Printf("[router] send to session %s: %v", chatID, err)
		_ = sender.SendMarkdown(ctx, chatID, "会话已断开，请重新发送消息。")
		r.drop(chatID)
	}
}

// sessionFor 返回 chatID 对应的会话，不存在则创建。
func (r *Router) sessionFor(ctx context.Context, chatID string) (*session.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.sessions[chatID]; ok {
		e.lastUsed = time.Now()
		return e.sess, nil
	}
	opts := r.opts.Session
	if r.opts.SharedWorkDir != "" {
		opts.Dir = r.opts.SharedWorkDir
	} else {
		opts.Dir = r.workDir(chatID)
	}
	// 尝试恢复上次（或上次进程生命周期内）的会话。
	opts.ResumeSessionID = r.loadSessionID(chatID)
	resumed := opts.ResumeSessionID
	startedAt := time.Now()
	opts.OnSession = func(id string) {
		r.saveSessionID(chatID, id)
	}
	opts.OnExit = func(err error) {
		if resumed != "" && err != nil && time.Since(startedAt) < resumeFailWindow {
			// 恢复失败，清除映射回退到新建会话。
			log.Printf("[router] resume %s failed, falling back to new session: %v", resumed, err)
			r.saveSessionID(chatID, "")
		}
		log.Printf("[router] session %s exited: %v", chatID, err)
		r.drop(chatID)
	}
	sess := session.New(opts)
	if err := sess.Start(ctx, r.frameHandler(chatID)); err != nil {
		return nil, err
	}
	r.sessions[chatID] = &entry{sess: sess, chatID: chatID, lastUsed: time.Now()}
	log.Printf("[router] session created: %s (total %d)", chatID, len(r.sessions))
	return sess, nil
}

// frameHandler 把会话输出回发到对应群聊。
func (r *Router) frameHandler(chatID string) session.Handler {
	return func(final bool, text string) {
		if !final || text == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := r.sender.SendMarkdown(ctx, chatID, text); err != nil {
			log.Printf("[router] deliver to %s: %v", chatID, err)
		} else {
			log.Printf("[router] delivered result to %s (%d bytes)", chatID, len(text))
		}
	}
}

// workDir 返回并确保会话独立工作目录存在。
func (r *Router) workDir(chatID string) string {
	if r.opts.WorkDirBase == "" {
		return ""
	}
	dir := fmt.Sprintf("%s/%s", r.opts.WorkDirBase, chatID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[router] create workdir %s: %v", dir, err)
		return ""
	}
	return dir
}

// dirFor 返回会话的工作目录（不创建）：共享工作区优先，其次按群子目录。
func (r *Router) dirFor(chatID string) string {
	if r.opts.SharedWorkDir != "" {
		return r.opts.SharedWorkDir
	}
	return r.workDir(chatID)
}

// parseFileCommand 解析 "/file [路径]" 命令（@机器人 前缀已在 Dispatch 剥离，
// 故要求命令位于文本开头，避免把提及该命令的普通对话误判为文件请求）。
// 返回 arg 为空表示 "/file" 不带参数。
func parseFileCommand(text string) (arg string, ok bool) {
	rest, ok := strings.CutPrefix(text, "/file")
	if !ok {
		return "", false
	}
	if rest != "" && !strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "\t") {
		return "", false // 避免误匹配 "/filexxx" 之类文本
	}
	return strings.TrimSpace(rest), true
}

// sendFile 把工作区文件上传并发送到会话。
func (r *Router) sendFile(ctx context.Context, sender *wecom.Client, reqID, chatID, arg string) {
	base := r.dirFor(chatID)
	if base == "" {
		_ = sender.ReplyText(ctx, reqID, "未配置工作目录，无法发送文件。")
		return
	}
	if arg == "" {
		_ = sender.ReplyText(ctx, reqID, "工作目录: "+base)
		return
	}
	full := filepath.Clean(arg)
	if !filepath.IsAbs(full) {
		full = filepath.Join(filepath.Clean(base), full)
	}
	// 只允许访问工作目录内的文件，禁止越界。
	if full != filepath.Clean(base) && !strings.HasPrefix(full, filepath.Clean(base)+string(os.PathSeparator)) {
		_ = sender.ReplyText(ctx, reqID, "只能发送工作区内的文件。")
		return
	}
	info, err := os.Stat(full)
	if err != nil {
		_ = sender.ReplyText(ctx, reqID, "文件不存在: "+arg)
		return
	}
	if info.IsDir() {
		_ = sender.ReplyText(ctx, reqID, "不能发送目录: "+arg)
		return
	}
	data, err := os.ReadFile(full)
	if err != nil {
		_ = sender.ReplyText(ctx, reqID, "读取失败: "+err.Error())
		return
	}
	_ = sender.ReplyText(ctx, reqID, "文件上传中…")
	mediaID, err := sender.UploadMedia(ctx, "file", filepath.Base(full), data)
	if err != nil {
		log.Printf("[router] upload %s: %v", full, err)
		_ = sender.SendMarkdown(ctx, chatID, "文件上传失败: "+err.Error())
		return
	}
	if err := sender.SendFile(ctx, chatID, mediaID); err != nil {
		log.Printf("[router] send file to %s: %v", chatID, err)
		_ = sender.SendMarkdown(ctx, chatID, "文件发送失败: "+err.Error())
		return
	}
	log.Printf("[router] file sent to %s: %s (%d bytes)", chatID, full, len(data))
}

// drop 删除并关闭会话。
func (r *Router) drop(chatID string) {
	r.mu.Lock()
	e, ok := r.sessions[chatID]
	delete(r.sessions, chatID)
	r.mu.Unlock()
	if ok {
		e.sess.Close()
	}
}

// Count 返回活跃会话数。
func (r *Router) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

// Close 关闭所有会话。
func (r *Router) Close() {
	r.mu.Lock()
	entries := make([]*entry, 0, len(r.sessions))
	for _, e := range r.sessions {
		entries = append(entries, e)
	}
	r.sessions = make(map[string]*entry)
	r.mu.Unlock()
	for _, e := range entries {
		e.sess.Close()
	}
}

// sessionMapPath 返回 chatID → session_id 映射文件路径：
// 共享工作区或 WorkDirBase 下，都未配置则放当前目录。
func (r *Router) sessionMapPath() string {
	switch {
	case r.opts.SharedWorkDir != "":
		return filepath.Join(r.opts.SharedWorkDir, sessionMapFile)
	case r.opts.WorkDirBase != "":
		return filepath.Join(r.opts.WorkDirBase, sessionMapFile)
	default:
		return sessionMapFile
	}
}

// loadSessionID 读取 chatID 上次使用的 session_id。
func (r *Router) loadSessionID(chatID string) string {
	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	data, err := os.ReadFile(r.sessionMapPath())
	if err != nil {
		return ""
	}
	var m map[string]string
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	return m[chatID]
}

// saveSessionID 记录（或传空清除）chatID 的 session_id。
func (r *Router) saveSessionID(chatID, id string) {
	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	path := r.sessionMapPath()
	m := make(map[string]string)
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	if id == "" {
		delete(m, chatID)
	} else {
		m[chatID] = id
	}
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("[router] persist session map: %v", err)
	}
}

// accept 抑制回调重投递（msgid 去重，带 TTL 与容量上限）。
func (r *Router) accept(messageID string) bool {
	now := time.Now()
	r.seenMu.Lock()
	defer r.seenMu.Unlock()
	cutoff := now.Add(-seenTTL)
	for id, seenAt := range r.seen {
		if seenAt.Before(cutoff) {
			delete(r.seen, id)
		}
	}
	if _, exists := r.seen[messageID]; exists {
		return false
	}
	if len(r.seen) >= seenMax {
		var oldestID string
		var oldest time.Time
		for id, seenAt := range r.seen {
			if oldestID == "" || seenAt.Before(oldest) {
				oldestID, oldest = id, seenAt
			}
		}
		delete(r.seen, oldestID)
	}
	r.seen[messageID] = now
	return true
}
