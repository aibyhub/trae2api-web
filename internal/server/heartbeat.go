// heartbeat.go 流式请求护线三件套：
//   - heartbeatWriter：轮换等待 + SSE 推流两阶段共享出口，互斥保证心跳与 chunk 不交错；
//   - runHeartbeat：上游静默期逐秒级发 SSE 注释（ ": ping" ），注释行不进事件流，
//     OpenAI 兼容 SDK 一律忽略；防客户端/中间代理读超时，
//     让「上游首字节慢」时客户端保持在线直到终态；
//   - writeSSETerminalError：轮换预算耗尽时的流内终态（event:error + OpenAI
//     错误对象 + [DONE]，一次 Write 保证原子性）。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// heartbeatWriter 带互斥的 ResponseWriter/Flusher 包装；Header 透传（SSE 头已提交），
// 不声明 WriteHeader（终态状态不可再改，调用方也不该再走 JSON 通道）。
type heartbeatWriter struct {
	mu sync.Mutex
	w  http.ResponseWriter
	fl http.Flusher
}

func (s *heartbeatWriter) Header() http.Header { return s.w.Header() }

func (s *heartbeatWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (s *heartbeatWriter) Flush() {
	s.mu.Lock()
	if s.fl != nil {
		s.fl.Flush()
	}
	s.mu.Unlock()
}

// WriteHeader 补齐 http.ResponseWriter 接口：接口达成用（流场景下头部已提交
// 或由 streamOpts 自写，此刻再写无副作用——net/http 对已提交头忽略二次 WriteHeader）。
func (s *heartbeatWriter) WriteHeader(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.w.WriteHeader(status)
}

// runHeartbeat 每 heartbeatInterval 发一条注释心跳，直到 done 关闭或写失败
// （客户端断开）。与 chunk/终态报错经由同一 mutex 出口，互不交错。
func runHeartbeat(w http.ResponseWriter, done <-chan struct{}) {
	fl, ok := w.(http.Flusher)
	if !ok {
		return
	}
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// writeSSETerminalError SSE 头已提交后的终态报错。写两条 data（裸错误字符串 + OpenAI
// 错误对象，兼容只认 data: 的 SDK）+ [DONE]；单次 Write 保证不被心跳打断。
func writeSSETerminalError(w io.Writer, msg string) {
	msgRaw, _ := json.Marshal(msg)
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    "no_healthy_account",
		},
	})
	var buf []byte
	buf = append(buf, []byte("event: error\ndata: ")...)
	buf = append(buf, msgRaw...)
	buf = append(buf, '\n', '\n')
	buf = append(buf, []byte("data: ")...)
	buf = append(buf, payload...)
	buf = append(buf, '\n', '\n')
	buf = append(buf, []byte("data: [DONE]\n\n")...)
	_, _ = w.Write(buf)
}
