// ops.go /admin/api 手动触发定时任务：签到（全部/单号）与 token 刷新（全部）。
// 写操作经 withAdminAuth（Bearer = TW2A_API_KEY）；Sched 未装配（测试/裸用）返回 501。
package server

import (
	"net/http"
	"sync"

	"trae2api-web/internal/scheduler"
)

// opsMu 「全部签到/全部刷新」互斥：签到幂等、并发无害，这里只挡住
// 连点两下造成的重复全量跑（UI 同时会禁用按钮，双保险）。
// 定时触发与手动触发的并发不在此列（上游幂等，池操作有锁）。
var opsMu sync.Mutex

// schedReady 校验 Sched 已装配；未装配写 501 并返回 false。
func schedReady(h *Handler, w http.ResponseWriter) bool {
	if h.cfg.Sched == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "no_scheduler", "scheduler not configured")
		return false
	}
	return true
}

// adminCheckinAll POST /admin/api/checkin_all：立即对所有账号签到 + 积分刷新 + 解冻。
// 同步执行（逐号请求上游），返回逐号结果与计数。
func (h *Handler) adminCheckinAll(w http.ResponseWriter, r *http.Request) {
	if !schedReady(h, w) {
		return
	}
	if !opsMu.TryLock() {
		writeOpenAIError(w, http.StatusConflict, "busy", "上一次签到仍在执行，请稍后刷新查看结果")
		return
	}
	defer opsMu.Unlock()
	results := h.cfg.Sched.RunCheckinNowManual()
	writeJSON(w, http.StatusOK, summarize(results))
}

// adminRefreshAll POST /admin/api/refresh_all：对快过期的账号刷新 token
// （尊重预刷新窗口，未到窗口的记 skipped），返回逐号结果与计数。
func (h *Handler) adminRefreshAll(w http.ResponseWriter, r *http.Request) {
	if !schedReady(h, w) {
		return
	}
	if !opsMu.TryLock() {
		writeOpenAIError(w, http.StatusConflict, "busy", "上一次操作仍在执行，请稍后")
		return
	}
	defer opsMu.Unlock()
	results := h.cfg.Sched.RunRefreshNow()
	writeJSON(w, http.StatusOK, summarize(results))
}

// adminCheckinOne POST /admin/api/accounts/{uid}/checkin：单账号立即签到。
func (h *Handler) adminCheckinOne(w http.ResponseWriter, r *http.Request) {
	if !schedReady(h, w) {
		return
	}
	uid := r.PathValue("uid")
	if h.cfg.Pool.AuthByUID(uid) == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "no auth for uid")
		return
	}
	res := h.cfg.Sched.CheckinUIDManual(uid)
	writeJSON(w, http.StatusOK, map[string]any{"result": res})
}

// summarize 把逐号结果聚合成前端好展示的计数（results 原样透出）。
func summarize(results []scheduler.Result) map[string]any {
	claimed, already, off, refreshed, skipped, failed := 0, 0, 0, 0, 0, 0
	for _, r := range results {
		switch r.Status {
		case "claimed":
			claimed++
		case "already":
			already++
		case "checkin_off":
			off++
		case "refreshed":
			refreshed++
		case "skipped":
			skipped++
		default:
			failed++
		}
	}
	return map[string]any{
		"total":       len(results),
		"claimed":     claimed,
		"already":     already,
		"checkin_off": off,
		"refreshed":   refreshed,
		"skipped":     skipped,
		"failed":      failed,
		"results":     results,
	}
}
