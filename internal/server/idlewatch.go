package server

// idlewatch.go 上游响应体「读空闲看门狗」：
//
// 上游 SOLO 在大 context / 后端劣化时会进入「头已到、SSE 无事件」的冻结态
// （生产实测：code=2001 流内挂 44 分钟；空心流 303~1256s，0 token 记 "ok"）。
// 这类冻结发生在响应头之后，upstream.Config 的 ResponseHeaderTimeout 管不到，
// 客户端又只看到一条 200 + 不再推进的流。看门狗在应用层兜底：
//
//	连续 streamIdle 无任何新字节 → 主动 Close 底层连接，让阻塞中的
//	Read 立刻返回错误 → 上层把该请求按「流内中断」记 error 终态。
//
// 正常长响应不受影响：每收到一个字节都重置计时（"读空闲"是相对上一字节的
// 间隔，不是总时长）；大 context 预填生产实测 ≤30s 出首事件，120s 阈值不误杀。

import (
	"io"
	"sync"
	"time"
)

// idleWatchReader 包装上游响应体：任意两次成功 Read 间隔 > idleTimeout 时,
// 由 AfterFunc 定时器关闭底层连接（阻塞中的 Read 即刻报错）。
// Close 由上层（StreamWithHooks / Aggregate 收尾）调用，一次性停表并透传底层。
type idleWatchReader struct {
	rc   io.ReadCloser
	idle time.Duration

	mu    sync.Mutex
	timer *time.Timer
	// killed 标记已经因空闲触发 close（之后 Read 返回的 err 就是这个断流动作产生的）。
	killed bool
}

func newIdleWatchReader(rc io.ReadCloser, idle time.Duration) *idleWatchReader {
	w := &idleWatchReader{rc: rc, idle: idle}
	w.reset()
	return w
}

// reset 在成功收到一条上游数据后重新布防计时器。
func (w *idleWatchReader) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(w.idle, func() {
		w.mu.Lock()
		if w.killed {
			w.mu.Unlock()
			return
		}
		w.killed = true
		rc := w.rc
		w.mu.Unlock()
		_ = rc.Close() // 用连接级关闭唤醒阻塞中的 Read
	})
}

// Read 透传底层并在成功后重置看门狗。
func (w *idleWatchReader) Read(p []byte) (int, error) {
	n, err := w.rc.Read(p)
	if n > 0 && err == nil {
		w.reset()
	}
	return n, err
}

// Close 停表 + 透传底层关闭（幂等安全，Stop 过的 timer 再 Stop 无副作用）。
func (w *idleWatchReader) Close() error {
	w.mu.Lock()
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.mu.Unlock()
	return w.rc.Close()
}

// Killed 报告该连接是否被看门狗断掉（区分「上游 EOF」与「空闲断流」），
// 流内报错文案与测试使用。
func (w *idleWatchReader) Killed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.killed
}
