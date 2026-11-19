// Package dashscope implements the Alibaba DashScope realtime ASR
// client (paraformer-realtime-v2) over the duplex WebSocket task
// protocol:
//
//	wss://dashscope.aliyuncs.com/api-ws/v1/inference
//
// Flow: run-task JSON -> task-started -> binary audio frames ->
// result-generated (partial/final sentences) -> finish-task ->
// task-finished. Auth is the API key on the upgrade handshake
// (User-Agent mandatory - bare clients get 401).
package dashscope

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/lixuanqun/gvos-core/internal/mediamain/mediastream"
)

// Config for the ASR session.
type Config struct {
	APIKey     string
	Model      string // default paraformer-realtime-v2
	Format     string // pcm | wav | opus ...
	SampleRate int    // 16000 typical
	Language   string // zh
	// Hot words / vocabulary-id etc. can be added as needed.
}

const wsURL = "wss://dashscope.aliyuncs.com/api-ws/v1/inference"

// Sentence is one recognition result: Text is the cumulative text of
// the current sentence; Final marks sentence end (a new sentence
// begins on the next event).
type Sentence struct {
	Text  string
	Final bool
}

// Session is one streaming recognition task.
type Session struct {
	cfg     Config
	conn    *mediastream.WSConn
	taskID  string
	mu      sync.Mutex
	closed  bool
	results chan Sentence
	errs    chan error
}

// NewSession dials, starts the task, and returns the live session.
// Consume results via Results() and errors via Err().
func NewSession(cfg Config) (*Session, error) {
	if cfg.Model == "" {
		cfg.Model = "paraformer-realtime-v2"
	}
	if cfg.Format == "" {
		cfg.Format = "pcm"
	}
	if cfg.SampleRate == 0 {
		cfg.SampleRate = 16000
	}
	if cfg.Language == "" {
		cfg.Language = "zh"
	}

	conn, err := mediastream.DialWS(wsURL, map[string]string{
		"Authorization": "Bearer " + cfg.APIKey,
		"X-DashScope-DataInspection": "enable",
	}, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	s := &Session{
		cfg:     cfg,
		conn:    conn,
		taskID:  fmt.Sprintf("gvos-%d", time.Now().UnixNano()),
		results: make(chan Sentence, 16),
		errs:    make(chan error, 4),
	}

	if err := s.runTask(); err != nil {
		conn.Close()
		return nil, err
	}
	go s.readLoop()
	return s, nil
}

type taskEnvelope struct {
	Header struct {
		Action    string `json:"action"`
		TaskID    string `json:"task_id"`
		Streaming string `json:"streaming"`
		Event     string `json:"event,omitempty"`
		ErrorCode string `json:"error_code,omitempty"`
		ErrorMsg  string `json:"error_message,omitempty"`
	} `json:"header"`
	Payload struct {
		TaskGroup  string            `json:"task_group"`
		Task       string            `json:"task"`
		Function   string            `json:"function"`
		Model      string            `json:"model"`
		Parameters map[string]any    `json:"parameters,omitempty"`
		Input      map[string]any    `json:"input"`
		Output     *struct {
			Sentence *struct {
				Text     string `json:"text"`
				EndTime  int64  `json:"end_time"`
				BeginNo  int    `json:"begin_no"`
			} `json:"sentence"`
		} `json:"output,omitempty"`
	} `json:"payload"`
	Context struct {
		SDK       string         `json:"sdk"`
		Attribute map[string]any `json:"attribute"`
	} `json:"context"`
}

func (s *Session) runTask() error {
	var env taskEnvelope
	env.Header.Action = "run-task"
	env.Header.TaskID = s.taskID
	env.Header.Streaming = "duplex"
	env.Payload.TaskGroup = "audio"
	env.Payload.Task = "asr"
	env.Payload.Function = "recognition"
	env.Payload.Model = s.cfg.Model
	env.Payload.Parameters = map[string]any{
		"format": s.cfg.Format, "sample_rate": s.cfg.SampleRate,
		"language": s.cfg.Language,
	}
	env.Payload.Input = map[string]any{}
	env.Context.SDK = "gvos"
	env.Context.Attribute = map[string]any{"workspace_id": "default"}

	blob, _ := json.Marshal(env)
	if err := s.conn.Write(mediastream.OpTextValue, blob); err != nil {
		return err
	}
	// Wait for task-started.
	s.conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	f, err := s.conn.Read()
	if err != nil {
		return fmt.Errorf("await task-started: %w", err)
	}
	var resp taskEnvelope
	if err := json.Unmarshal(f.Payload, &resp); err != nil {
		return fmt.Errorf("task-started parse: %w", err)
	}
	switch resp.Header.Event {
	case "task-started":
		return nil
	case "task-failed":
		return fmt.Errorf("task-failed: %s %s", resp.Header.ErrorCode, resp.Header.ErrorMsg)
	default:
		return fmt.Errorf("unexpected first event: %q", resp.Header.Event)
	}
}

// PushAudio sends one chunk of raw audio bytes.
func (s *Session) PushAudio(b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("session closed")
	}
	return s.conn.Write(mediastream.OpBinaryValue, b)
}

// Finish sends finish-task. Results may still arrive for a short
// window afterwards.
func (s *Session) Finish() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	var env taskEnvelope
	env.Header.Action = "finish-task"
	env.Header.TaskID = s.taskID
	env.Header.Streaming = "duplex"
	env.Payload.Input = map[string]any{}
	env.Context.SDK = "gvos"
	env.Context.Attribute = map[string]any{"workspace_id": "default"}
	blob, _ := json.Marshal(env)
	return s.conn.Write(mediastream.OpTextValue, blob)
}

// Close tears the session down (also unblocks Results readers via
// channel close).
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.conn.Close()
	close(s.results)
	close(s.errs)
}

// Results is the sentence stream; closed on Close.
func (s *Session) Results() <-chan Sentence { return s.results }

// Err returns the error channel (task failures surfaced here).
func (s *Session) Err() <-chan error { return s.errs }

func (s *Session) readLoop() {
	defer func() {
		s.mu.Lock()
		wasClosed := s.closed
		s.mu.Unlock()
		if !wasClosed {
			s.Close()
		}
	}()
	for {
		s.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		f, err := s.conn.Read()
		if err != nil {
			select {
			case s.errs <- err:
			default:
			}
			return
		}
		switch f.Opcode {
		case mediastream.OpTextValue:
			var env taskEnvelope
			if err := json.Unmarshal(f.Payload, &env); err != nil {
				continue
			}
			switch env.Header.Event {
			case "result-generated":
				sent := env.Payload.Output.Sentence
				if sent == nil {
					continue
				}
				final := sent.EndTime > 0 || sent.BeginNo != 0
				select {
				case s.results <- Sentence{Text: sent.Text, Final: final}:
				default: // slow consumer drops partials, keeps finals queued next
				}
			case "task-finished":
				return
			case "task-failed":
				select {
				case s.errs <- fmt.Errorf("task-failed: %s %s", env.Header.ErrorCode, env.Header.ErrorMsg):
				default:
				}
				return
			}
		case mediastream.OpCloseValue:
			return
		}
	}
}
