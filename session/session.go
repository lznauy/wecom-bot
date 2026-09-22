// Package session 管理一个群聊对应的独立会话：一个 nekocode --headless 子进程，
// 通过 NDJSON over stdio 双向通信（协议见 NekoCode docs/STREAM_JSON.md）。
package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Handler 接收子进程产出的文本输出。
// final 为 true 时表示一个 turn 的最终 result；false 时为 assistant 中间文本。
type Handler func(final bool, text string)

// Options 控制子进程行为。
type Options struct {
	// Bin 是 nekocode CLI 可执行文件路径（如 nekocode-tui）。
	Bin string
	// Dir 是子进程工作目录（每个会话可独立，实现状态隔离）。
	Dir string
	// ResumeSessionID 非空时以 --resume 恢复已有会话。
	ResumeSessionID string
	// OnSession 在获知当前 session_id（含恢复后、会话切换）时回调。
	OnSession func(id string)
	// OnExit 在子进程意外退出时回调（ctx 取消导致的正常退出不回调）。
	OnExit func(err error)
}

// Session 是一个子进程会话。并发安全。
type Session struct {
	opts Options

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	cancel context.CancelFunc

	writeMu   sync.Mutex
	seenMu    sync.Mutex
	seen      map[string]bool // user message uuid 去重
	sessionMu sync.Mutex
	sessionID string // 当前会话 ID，从子进程帧中提取

	closed bool
}

// New 创建但并不启动会话。
func New(opts Options) *Session {
	if opts.Bin == "" {
		opts.Bin = "nekocode-tui"
	}
	return &Session{opts: opts, seen: make(map[string]bool)}
}

// Start 启动子进程并开始读取输出。onFrame 回调在独立 goroutine 中执行。
func (s *Session) Start(ctx context.Context, onFrame Handler) error {
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	args := []string{"--headless"}
	if s.opts.ResumeSessionID != "" {
		args = append(args, "--resume", s.opts.ResumeSessionID)
	}
	cmd := exec.CommandContext(runCtx, s.opts.Bin, args...)
	if s.opts.Dir != "" {
		cmd.Dir = s.opts.Dir
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start %s: %w", s.opts.Bin, err)
	}
	s.cmd = cmd
	s.stdin = stdin

	go func() {
		err := s.readLoop(runCtx, stdout, onFrame)
		waitErr := cmd.Wait()
		cancel()
		if ctx.Err() == nil && !s.isClosed() && s.opts.OnExit != nil {
			if err != nil {
				s.opts.OnExit(err)
			} else if waitErr != nil {
				s.opts.OnExit(waitErr)
			}
		}
	}()
	return nil
}

func (s *Session) isClosed() bool {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	return s.closed
}

func (s *Session) readLoop(ctx context.Context, stdout io.Reader, onFrame Handler) error {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024) // 协议单帧上限 8 MiB，留余量
	for sc.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var frame struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			Server    struct {
				SessionID string `json:"session_id"`
			} `json:"server"`
		}
		if json.Unmarshal(line, &frame) == nil {
			id := frame.SessionID
			if id == "" {
				id = frame.Server.SessionID
			}
			if id != "" {
				s.setSessionID(id)
			}
		}
		var typed struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &typed) != nil {
			continue
		}
		switch typed.Type {
		case "assistant":
			var f struct {
				Message struct {
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &f) == nil {
				for _, blk := range f.Message.Content {
					if blk.Type == "text" && strings.TrimSpace(blk.Text) != "" && onFrame != nil {
						onFrame(false, blk.Text)
					}
				}
			}
		case "result":
			var f struct {
				IsError bool   `json:"is_error"`
				Result  string `json:"result"`
			}
			if json.Unmarshal(line, &f) == nil && onFrame != nil {
				text := f.Result
				if f.IsError && text == "" {
					text = "执行出错，请稍后重试。"
				}
				onFrame(true, text)
			}
		case "control_request":
			s.answerControl(line)
		}
	}
	return sc.Err()
}

func (s *Session) setSessionID(id string) {
	s.sessionMu.Lock()
	changed := s.sessionID != id
	s.sessionID = id
	s.sessionMu.Unlock()
	if changed && s.opts.OnSession != nil {
		s.opts.OnSession(id)
	}
}

// CurrentSession 返回当前已知的 session_id（尚未获知时为空）。
func (s *Session) CurrentSession() string {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	return s.sessionID
}

// answerControl 自动应答服务端反向请求：审批按配置放行或拒绝，提问一律拒绝。
func (s *Session) answerControl(line []byte) {
	var f struct {
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype string `json:"subtype"`
		} `json:"request"`
	}
	if json.Unmarshal(line, &f) != nil || f.RequestID == "" {
		return
	}
	resp := map[string]any{
		"subtype":    "success",
		"request_id": f.RequestID,
	}
	switch f.Request.Subtype {
	case "can_use_tool":
		// 群聊机器人场景无人工审批环节，工具调用一律放行（仅当次请求，不落权限规则）。
		resp["response"] = map[string]any{"behavior": "allow"}
	case "nekocode_question":
		resp["response"] = map[string]any{"rejected": true}
	default:
		return
	}
	s.send(map[string]any{"type": "control_response", "response": resp})
}

// Send 提交一轮用户输入。uuid 需在同一会话内唯一。
func (s *Session) Send(uuid, text string) error {
	s.seenMu.Lock()
	if s.closed || s.seen[uuid] {
		s.seenMu.Unlock()
		if s.closed {
			return fmt.Errorf("session closed")
		}
		return nil
	}
	s.seen[uuid] = true
	s.seenMu.Unlock()
	return s.send(map[string]any{
		"type":    "user",
		"uuid":    uuid,
		"message": map[string]any{"role": "user", "content": text},
	})
}

func (s *Session) send(obj any) error {
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.stdin == nil {
		return fmt.Errorf("session not started")
	}
	if _, err := s.stdin.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

// Close 终止子进程（SIGKILL 由 CommandContext 取消触发）。
func (s *Session) Close() {
	s.seenMu.Lock()
	if s.closed {
		s.seenMu.Unlock()
		return
	}
	s.closed = true
	s.seenMu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
}
