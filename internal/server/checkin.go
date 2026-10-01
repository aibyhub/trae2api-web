// checkin.go /admin/api/checkin：面板「自动签到」看板。
//
// 回答「每天的自动签到到底正常吗」：今日每个账号的最终结果、需要关注的账号、
// 自动/手动成功率、近 7 天日报、下次签到窗口与最近明细。
package server

import (
	"net/http"
	"time"

	"trae2api-web/internal/scheduler"
)

// adminCheckinBoard GET /admin/api/checkin
func (h *Handler) adminCheckinBoard(w http.ResponseWriter, r *http.Request) {
	if !schedReady(h, w) {
		return
	}
	now := time.Now()
	todays := h.cfg.Sched.CheckinsSince(scheduler.StartOfToday(now))
	latest := map[string]scheduler.CheckinLogEntry{}
	for _, e := range todays {
		latest[e.UID] = e
	}

	type accRow struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname,omitempty"`
		Status   string `json:"status"` // claimed/already/error/checkin_off/skipped/none
		At       int64  `json:"at,omitempty"`
		Error    string `json:"error,omitempty"`
		Manual   bool   `json:"manual,omitempty"`
	}

	rows := make([]accRow, 0, 8)
	pending := make([]accRow, 0, 4)
	total, signed, claimed, already, errN, off, skip, none := 0, 0, 0, 0, 0, 0, 0, 0
	for _, st := range h.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		total++
		row := accRow{UID: st.UID, Nickname: st.Nickname}
		e, ok := latest[st.UID]
		if !ok {
			row.Status = "none"
			none++
			pending = append(pending, row)
		} else {
			row.Status, row.At, row.Error, row.Manual = e.Status, e.TS, e.Error, e.Manual
			switch e.Status {
			case "claimed":
				claimed++
				signed++
			case "already":
				already++
				signed++
			case "error":
				errN++
				pending = append(pending, row)
			case "checkin_off":
				off++
			default:
				skip++
			}
		}
		rows = append(rows, row)
	}

	// 今日原始记录里的自动 / 手动成功率：避免"手动点过"掩盖"定时任务没跑"
	at, aok, afail, mt, mok, mfail := 0, 0, 0, 0, 0, 0
	var autoLast int64
	for _, e := range todays {
		ok := e.Status == "claimed" || e.Status == "already"
		if e.Manual {
			mt++
			if ok {
				mok++
			} else {
				mfail++
			}
			continue
		}
		at++
		if ok {
			aok++
		} else {
			afail++
		}
		if e.TS > autoLast {
			autoLast = e.TS
		}
	}
	rate := func(ok, tot int) float64 {
		if tot == 0 {
			return 0
		}
		return float64(int(float64(ok)/float64(tot)*1000+0.5)) / 10
	}

	// 近 7 天日报（原始记录按本地日期分组）
	week := h.cfg.Sched.CheckinsSince(now.AddDate(0, 0, -6).Unix())
	byDay := map[string]map[string]int{}
	for _, e := range week {
		d := time.Unix(e.TS, 0).Format("2006-01-02")
		m := byDay[d]
		if m == nil {
			m = map[string]int{}
			byDay[d] = m
		}
		m["entries"]++
		switch e.Status {
		case "claimed":
			m["claimed"]++
		case "already":
			m["already"]++
		case "error":
			m["error"]++
		case "checkin_off":
			m["off"]++
		default:
			m["skipped"]++
		}
	}
	days := make([]map[string]any, 0, 7)
	for i := 6; i >= 0; i-- {
		d := now.AddDate(0, 0, -i).Format("2006-01-02")
		m := byDay[d]
		if m == nil {
			m = map[string]int{}
		}
		days = append(days, map[string]any{
			"date": d, "entries": m["entries"], "ok": m["claimed"] + m["already"],
			"claimed": m["claimed"], "already": m["already"],
			"error": m["error"], "off": m["off"], "skipped": m["skipped"],
		})
	}

	nextAt, windowEnd := h.cfg.Sched.NextCheckinWindow()
	recent := h.cfg.Sched.RecentCheckins(40)
	if recent == nil {
		recent = []scheduler.CheckinLogEntry{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"date":           now.Format("2006-01-02"),
		"now":            now.Unix(),
		"hours":          h.cfg.Sched.CheckinHours(),
		"jitter_minutes": h.cfg.Sched.JitterMinutes(),
		"next_at":        nextAt.Unix(),
		"window_end":     windowEnd.Unix(),
		"auto_last_at":   autoLast,
		"notify": map[string]any{
			"configured": h.cfg.Sched.NotifyConfigured(),
			"mode":       h.cfg.Sched.NotifyModeName(),
		},
		"stats": map[string]any{
			"total": total, "signed": signed, "claimed": claimed, "already": already,
			"error": errN, "off": off, "skipped": skip, "none": none, "pending": len(pending),
			"auto":   map[string]any{"total": at, "ok": aok, "fail": afail, "rate": rate(aok, at)},
			"manual": map[string]any{"total": mt, "ok": mok, "fail": mfail, "rate": rate(mok, mt)},
		},
		"accounts": rows,
		"pending":  pending,
		"days":     days,
		"recent":   recent,
	})
}
