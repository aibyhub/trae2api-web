// charts.go /admin/api/charts：面板「使用分析」页数据源（只读聚合，无鉴权与
// usage/accounts 同级）。
//
//	daily    近 14 天调用量：从 data/call-details/call-details-YYYY-MM-DD.jsonl
//	         聚合 status ok/error 计数 + 当日按账号成功分布（3k+ 次/天时为一次纯计数扫描）。
//	credits  积分余额轨迹：data/credits.jsonl 变更点（变更时点的余额即历史余额）+ 当前值，
//	         附近 7 天签到收益与近 3 天消耗速率（"还能撑几天"）。
//	expiry   权益包到期分布：pool 快照（expire_at / credits / 分包 limit-used 与到期时刻）。
package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
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
// dailySum 单日调用量摘要（data/chartdaily/<day>.json 侧车）。
// chartDailyCalls 的数据源：明细文件只扫「自上次的字节偏移」起的增量，
// 面板 14 天直方图不再对数百 MB 的 call-details 大文件重复全量 JSON 解码
// （2026-10-18 实测 6 文件合计 1.2GB 时 charts 42s → 毫秒级）。
type dailySum struct {
	Date     string           `json:"date"`
	Consumed int64            `json:"consumed"` // 已计入摘要的明细字节数（行边界对齐）
	OK       int64            `json:"ok"`
	Error    int64            `json:"error"`
	MaxMs    int64            `json:"max_ms,omitempty"` // 当日最慢单笔（>0 才输出）
	ByUID    map[string]int64 `json:"by_uid,omitempty"`
}

func detailDayPath(dataDir, day string) string {
	return filepath.Join(dataDir, "call-details", "call-details-"+day+".jsonl")
}

func dailySumPath(dataDir, day string) string {
	return filepath.Join(dataDir, "chartdaily", day+".json")
}

// scanDetailChunk 从 offset（行边界对齐）起扫到文件末尾，只解析完整行；
// 返回新的 consumed 偏移。结尾整行未终结时最后半行留待下一批。
func scanDetailChunk(f *os.File, offset int64, add func(uid, status string, ms int64)) (int64, error) {
	fi, err := f.Stat()
	if err != nil {
		return offset, err
	}
	if fi.Size() <= offset {
		return offset, nil
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	raw := make([]byte, fi.Size()-offset)
	if _, err := io.ReadFull(f, raw); err != nil {
		return offset, err
	}
	nl := bytes.LastIndexByte(raw, '\n')
	if nl < 0 {
		return offset, nil // 无完整行（写入者在打断），等下次增量
	}
	var rec struct {
		UID        string `json:"uid"`
		Status     string `json:"status"`
		DurationMs int64  `json:"duration_ms"`
	}
	sc := bufio.NewScanner(bytes.NewReader(raw[:nl+1]))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		add(rec.UID, rec.Status, rec.DurationMs)
	}
	return offset + int64(nl) + 1, nil
}

func loadDailySumFile(dataDir, day string) *dailySum {
	raw, err := os.ReadFile(dailySumPath(dataDir, day))
	if err != nil {
		return nil
	}
	var s dailySum
	if json.Unmarshal(raw, &s) != nil || s.Date != day || s.Consumed < 0 {
		return nil
	}
	if s.ByUID == nil {
		s.ByUID = map[string]int64{}
	}
	return &s
}

func saveDailySumFile(dataDir, day string, s *dailySum) {
	dir := filepath.Dir(dailySumPath(dataDir, day))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return
	}
	tmp := dailySumPath(dataDir, day+".tmp")
	if os.WriteFile(tmp, raw, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, dailySumPath(dataDir, day))
}

// dailySummary 取某日摘要：优先读侧车文件，明细文件自 offset 起
// 增量吸收新增行；文件被裁剪去头（size < consumed）时整日重扫；
// 文件缺失/空转（轮转删除）时保留历史计数。
func (h *Handler) dailySummary(day string) *dailySum {
	h.chartMu.Lock()
	defer h.chartMu.Unlock()
	if h.chartCache == nil {
		h.chartCache = map[string]*dailySum{}
	}
	s := h.chartCache[day]
	if s == nil {
		s = loadDailySumFile(h.cfg.DataDir, day)
	}
	if s == nil {
		s = &dailySum{Date: day, ByUID: map[string]int64{}}
	}
	s.Date = day
	h.chartCache[day] = s

	fi, err := os.Stat(detailDayPath(h.cfg.DataDir, day))
	if err != nil || fi.Size() == 0 {
		return s // 未产生 / 已被轮转删除 / 空文件：摘要照旧
	}
	size := fi.Size()
	if size < s.Consumed && s.Consumed > 0 {
		// 明细被裁剪到摘要起点之前 → 整日重扫
		*s = dailySum{Date: day, ByUID: map[string]int64{}}
	}
	if size == s.Consumed {
		return s
	}
	f, err := os.Open(detailDayPath(h.cfg.DataDir, day))
	if err != nil {
		return s
	}
	defer f.Close()
	off, err := scanDetailChunk(f, s.Consumed, func(uid, status string, ms int64) {
		if status == "ok" {
			s.OK++
			if uid != "" {
				s.ByUID[uid]++
			}
			if ms > s.MaxMs {
				s.MaxMs = ms
			}
		} else {
			s.Error++
		}
	})
	if err != nil || off <= s.Consumed {
		return s
	}
	s.Consumed = off
	if fi.Size() >= s.Consumed { // 文件已达 consumed 前端（全量读完）才持久化摘要
		saveDailySumFile(h.cfg.DataDir, day, s)
	}
	return s
}

func (h *Handler) chartDailyCalls() []dailyCalls {
	now := time.Now()
	out := make([]dailyCalls, 0, chartDays)
	for i := chartDays - 1; i >= 0; i-- {
		day := now.AddDate(0, 0, -i).Format("2006-01-02")
		s := h.dailySummary(day)
		byUID := make(map[string]int, len(s.ByUID))
		for uid, c := range s.ByUID {
			byUID[uid] = int(c)
		}
		out = append(out, dailyCalls{Date: day, OK: int(s.OK), Error: int(s.Error), MaxMs: s.MaxMs, ByUID: byUID})
	}
	return out
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
