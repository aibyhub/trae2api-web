// logs.go 数据维护：日志概况 + 按保留期裁剪（usage.jsonl / checkin.jsonl）。
//
// 日志不能无限长大：默认保留 90 天（TW2A_LOG_RETENTION_DAYS 可调，0 = 关闭自动清理），
// 启动时清理一次、之后每 24h 一次；面板「使用记录」页底部可查看概况并手动清理。
package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

// logMaintenance 最近一次日志清理结果。
type logMaintenance struct {
	mu       sync.Mutex
	at       int64
	days     int
	removedU int
	removedC int
	errMsg   string
}

func (m *logMaintenance) set(days, removedU, removedC int, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.at, m.days, m.removedU, m.removedC, m.errMsg = time.Now().Unix(), days, removedU, removedC, errMsg
}

func (m *logMaintenance) snapshot() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return map[string]any{
		"at":              m.at,
		"days":            m.days,
		"removed_usage":   m.removedU,
		"removed_checkin": m.removedC,
		"error":           m.errMsg,
	}
}

// logFileInfo 单个日志文件概况（名称 / 大小 / 条数 / 最早最新时间）。
func (h *Handler) logFileInfo(name, path string, entries int, oldest, newest int64) map[string]any {
	var size int64
	if st, err := os.Stat(path); err == nil {
		size = st.Size()
	}
	return map[string]any{
		"name": name, "size_bytes": size, "entries": entries,
		"oldest_at": oldest, "newest_at": newest,
	}
}

// logFiles 汇总两类日志的概况（checkin 未装配调度器时跳过）。
func (h *Handler) logFiles() []map[string]any {
	out := make([]map[string]any, 0, 2)
	ue, uo, un := h.usage.Stats()
	out = append(out, h.logFileInfo("usage.jsonl", h.usage.Path(), ue, uo, un))
	if h.cfg.Sched != nil {
		ce, co, cn := h.cfg.Sched.CheckinLogStats()
		out = append(out, h.logFileInfo("checkin.jsonl", h.cfg.Sched.CheckinLogPath(), ce, co, cn))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["name"].(string) < out[j]["name"].(string)
	})
	return out
}

// adminLogsGet GET /admin/api/logs：日志概况 + 保留策略 + 最近一次清理结果。
func (h *Handler) adminLogsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"retention_days": h.cfg.LogRetentionDays,
		"files":          h.logFiles(),
		"last_prune":     h.logs.snapshot(),
	})
}

// adminLogsPrune POST /admin/api/logs/prune {days} 或 {clear:true}：
// 手动按保留期清理（days>0 保留最近 N 天；clear=true 清空全部历史）。
func (h *Handler) adminLogsPrune(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Days  int  `json:"days"`
		Clear bool `json:"clear"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "decode body: "+err.Error())
		return
	}
	if !req.Clear && req.Days <= 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "days 必须为正数（或传 clear=true 清空）")
		return
	}
	days := req.Days
	if req.Clear {
		days = 0
	}
	ru, rc, errMsg := h.PruneLogs(days)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": errMsg == "", "removed_usage": ru, "removed_checkin": rc, "error": errMsg,
		"files": h.logFiles(), "last_prune": h.logs.snapshot(),
	})
}

// PruneLogs 按保留期裁剪两类日志，返回删除条数与错误信息。
// keepDays：<0 不动作 / 0 清空 / >0 保留最近 N 天。
func (h *Handler) PruneLogs(keepDays int) (removedUsage, removedCheckin int, errMsg string) {
	if keepDays < 0 {
		return 0, 0, ""
	}
	ru, _, err := h.usage.Prune(keepDays)
	if err != nil {
		errMsg = "usage: " + err.Error()
	}
	rc := 0
	if h.cfg.Sched != nil {
		r, _, cerr := h.cfg.Sched.PruneCheckins(keepDays)
		rc = r
		if cerr != nil {
			if errMsg != "" {
				errMsg += "; "
			}
			errMsg += "checkin: " + cerr.Error()
		}
	}
	h.logs.set(keepDays, ru, rc, errMsg)
	return ru, rc, errMsg
}

// StartLogRetention 启动时清理一次，之后每 24h 一次（LogRetentionDays<=0 关闭）。
func (h *Handler) StartLogRetention(ctx context.Context) {
	days := h.cfg.LogRetentionDays
	if days <= 0 {
		log.Printf("log retention disabled (TW2A_LOG_RETENTION_DAYS=0)")
		return
	}
	prune := func() {
		ru, rc, errMsg := h.PruneLogs(days)
		log.Printf("log retention: keep %d days → removed usage=%d checkin=%d err=%q", days, ru, rc, errMsg)
	}
	go func() {
		prune()
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				prune()
			}
		}
	}()
}
