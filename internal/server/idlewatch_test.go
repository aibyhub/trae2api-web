package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"trae2api-web/internal/auth"
	"trae2api-web/internal/upstream"
)

// stalledBody 模拟「SSE 中途冻结」的响应体：Read 一直阻塞，直到 Close() 触发
// shutdown → errCh 注入 io.ErrClosedPipe（等价于看门狗关闭连接后 Read 立即报错）。
type stalledBody struct {
	errCh    chan error
	shutdown chan struct{}
	closed   bool
}

func newStalledBody() *stalledBody {
	s := &stalledBody{errCh: make(chan error, 1), shutdown: make(chan struct{})}
	go func() {
		<-s.shutdown
		s.errCh <- io.ErrClosedPipe
	}()
	return s
}

func (s *stalledBody) Read([]byte) (int, error) { return 0, <-s.errCh }
func (s *stalledBody) Close() error {
	if !s.closed {
		s.closed = true
		close(s.shutdown)
	}
	return nil
}

// TestIdleWatchReaderKillsStalledBody 看门狗单元：150ms 无字节 → 关闭底层 →
// 阻塞中的 Read 立刻返回错误（而不是悬挂到超时兜底）。
func TestIdleWatchReaderKillsStalledBody(t *testing.T) {
	w := newIdleWatchReader(newStalledBody(), 150*time.Millisecond)
	start := time.Now()
	buf := make([]byte, 4)
	_, err := w.Read(buf)
	d := time.Since(start)
	if err == nil {
		t.Fatal("Read must return error after idle kill")
	}
	if d > 3*time.Second {
		t.Fatalf("idle kill too slow: %v", d)
	}
	if !w.Killed() {
		t.Fatal("Killed() must be true after watchdog fired")
	}
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
}

// TestIdleWatchReaderTrickleHolds 正常慢速逐字节流（快于阈值）不应触发看门狗。
func TestIdleWatchReaderTrickleHolds(t *testing.T) {
	w := newIdleWatchReader(&slowReader{delay: 40 * time.Millisecond, chunks: 8}, 150*time.Millisecond)
	defer func() { _ = w.Close() }()
	buf := make([]byte, 8)
	read := 0
	for {
		n, err := w.Read(buf)
		read += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
	}
	if w.Killed() {
		t.Fatal("byte trickle must keep resetting the watchdog")
	}
	if read == 0 {
		t.Fatal("no bytes read")
	}
}

type slowReader struct {
	delay  time.Duration
	chunks int
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.chunks == 0 {
		return 0, io.EOF
	}
	time.Sleep(s.delay)
	s.chunks--
	return copy(p, "chunk."), nil
}

func (s *slowReader) Close() error { return nil }

// preBuffer 先吐 head 字节再交给 rest（冻结体）。
type preBuffer struct {
	head string
	rest io.ReadCloser
}

func (p *preBuffer) Read(b []byte) (int, error) {
	if p.head != "" {
		n := copy(b, p.head)
		p.head = p.head[n:]
		return n, nil
	}
	return p.rest.Read(b)
}

func (p *preBuffer) Close() error {
	_ = p.rest.Close()
	return nil
}

// TestChatStreamIdleMidStreamTerminal 集成：SSE 头已提交后上游冻结 → 看门狗断流 →
// 客户端在流内收到 error 终态 + [DONE]（此前该场景静默假 "ok" / 挂 transport 超时）。
func TestChatStreamIdleMidStreamTerminal(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       &preBuffer{head: soloSSE[:40], rest: newStalledBody()},
			}, nil
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
	h := NewHandler(Config{
		Pool:              testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:          up,
		DataDir:           t.TempDir(),
		StreamIdleTimeout: 150 * time.Millisecond,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	start := time.Now()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("idle kill took too long: %v", d)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("want in-stream terminal error + [DONE], body=%s", body)
	}
	if !strings.Contains(body, "看门狗断流") {
		t.Fatalf("want watchdog message in terminal error, body=%s", body)
	}
}
