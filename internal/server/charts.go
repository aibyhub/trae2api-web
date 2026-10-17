// charts.go /admin/api/charts：面板「使用分析」页数据源（只读聚合，无鉴权与
// usage/accounts 同级）。
//
//	daily    近 14 天调用量：从 data/call-details/call-details-YYYY-MM-DD.jsonl
//	         聚合 status ok/error 计数 + 当日按账号成功分布（3k+ 次/天时为一次纯计数扫描）。
//	credits  积分余额轨迹：data/credits.jsonl 变更点（变更时点的余额即历史余额）+ 当前值，
//	         附近 7 天签到收益与近 3 天消耗速率（"还能撑几天"）。
//	expiry   权益包到期日历：pool 快照（expire_at / credits / 分包 limit-used 与到期时刻）。
package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// chartDays 调用量直方图与余额轨迹窗口（天）。
const chartDays = 14

type dailyCalls struct {
	Date  string         `json:"date"`
	OK    int            `json:"ok"`
	Error int            `json:"error"`
	MaxMs int64          `json:"max_ms,omitempty"` // 当日最慢单笔（>0 才输出）
	ByUID map[string]int `json:"by_uid"`           // 当日按账号成功调用数
}

type chartPoint struct {
	TS      int64 `json:"ts"` // unix 秒
	Credits int64 `json:"credits"`
}

type creditSeries struct {
	UID      string       `json:"uid"`
	Current  int64        `json:"current"`
	Points   []chartPoint `json:"points"`              // 窗口内的余额变更点（时间升序）
	Gained7d int64        `json:"gained_7d,omitempty"` // 近 7 天签到等来源的加分合计
	Burned3d int64        `json:"burned_3d,omitempty"` // 近 3 天扣分合计（正数）
	DaysLeft int64        `json:"days_left,omitempty"` // 当前余额 / 日均烧分（不动用=无限，0 = 未知）
}

func (h *Handler) adminCharts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"days":    chartDays,
		"daily":   h.chartDailyCalls(),
		"credits": h.chartCreditSeries(),
		"expiry":  h.chartExpiry(),
		"now":     time.Now().Unix(),
	})
}

// chartDailyCalls 近 14 天逐日调用量（本机时区日期，与 call-details 文件命名一致）。
func (h *Handler) chartDailyCalls() []dailyCalls {
	now := time.Now()
	out := make([]dailyCalls, 0, chartDays)
	for i := chartDays - 1; i >= 0; i-- {
		day := now.AddDate(0, 0, -i).Format("2006-01-02")
		d := dailyCalls{Date: day, ByUID: map[string]int{}}
		h.scanDetailDay(day, func(uid, status string, ms int64) {
			if status == "ok" {
				d.OK++
				if uid != "" {
					d.ByUID[uid]++
				}
				if ms > d.MaxMs {
					d.MaxMs = ms
				}
			} else {
				d.Error++
			}
		})
		out = append(out, d)
	}
	return out
}

func (h *Handler) scanDetailDay(day string, visit func(uid, status string, ms int64)) {
	f, err := os.Open(filepath.Join(h.cfg.DataDir, "call-details", "call-details-"+day+".jsonl"))
	if err != nil {
		return
	}
	defer f.Close()
	var rec struct {
		UID        string `json:"uid"`
		Status     string `json:"status"`
		DurationMs int64  `json:"duration_ms"`
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		visit(rec.UID, rec.Status, rec.DurationMs)
	}
}

// chartCreditSeries 各账号近 14 天余额变更点 + 当前值（credits.jsonl；变更点即历史余额快照）。
// 另算近 7 天加分（签到等）与近 3 天扣分速率 → days_left（撑几天）。
func (h *Handler) chartCreditSeries() []creditSeries {
	type credRec struct {
		TS      int64  `json:"ts"`
		UID     string `json:"uid"`
		Credits int64  `json:"credits"`
		Delta   int64  `json:"delta"`
		Source  string `json:"source"`
	}
	raw := map[string][]chartPoint{}
	now := time.Now().Unix()
	cutoff := now - chartDays*86400
	cut7 := now - 7*86400
	cut3 := now - 3*86400
	gains := map[string]int64{} // uid → 近 7 天加分
	burns := map[string]int64{} // uid → 近 3 天扣分（正数）
	if f, err := os.Open(filepath.Join(h.cfg.DataDir, "credits.jsonl")); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var l credRec
			if json.Unmarshal(sc.Bytes(), &l) != nil || l.TS == 0 || l.UID == "" || l.TS < cutoff {
				continue
			}
			raw[l.UID] = append(raw[l.UID], chartPoint{TS: l.TS, Credits: l.Credits})
			if l.Delta > 0 && l.TS >= cut7 {
				gains[l.UID] += l.Delta
			}
			if l.Delta < 0 && l.TS >= cut3 {
				burns[l.UID] += -l.Delta
			}
		}
		f.Close()
	}
	out := make([]creditSeries, 0, len(raw))
	for _, st := range h.cfg.Pool.List() {
		s := creditSeries{UID: st.UID, Current: st.Credits, Gained7d: gains[st.UID], Burned3d: burns[st.UID]}
		s.Points = append(s.Points, raw[st.UID]...)
		sort.Slice(s.Points, func(i, j int) bool { return s.Points[i].TS < s.Points[j].TS })
		s.Points = append(s.Points, chartPoint{TS: now, Credits: st.Credits})
		if daily := s.Burned3d / 3; daily > 0 {
			s.DaysLeft = st.Credits / daily
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

func (h *Handler) chartExpiry() []map[string]any {
	out := make([]map[string]any, 0)
	for _, st := range h.cfg.Pool.List() {
		out = append(out, map[string]any{
			"uid":       st.UID,
			"nickname":  st.Nickname,
			"credits":   st.Credits,
			"expire_at": st.ExpireAt,
			"packs":     st.Packs,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := out[i]["expire_at"].(int64)
		b, _ := out[j]["expire_at"].(int64)
		return a < b
	})
	return out
}
