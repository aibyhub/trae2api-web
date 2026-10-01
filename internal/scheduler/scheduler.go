// Package scheduler 定时任务：每日签到 + token 预刷新。
// 签到成功后重新查积分，积分 > 0 的冷却账号自动解冻。
// 签到时刻在 CheckinHour 后按每账号独立的随机 0~JitterMinutes 延迟执行
// （每日重掷、账号间互不相同），让上游看到的时间分布更像人操作。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"strings"
	"time"

	"trae2api-web/internal/pool"
	"trae2api-web/internal/upstream"
)

// Config 调度器依赖。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	CheckinHours []int // 每日签到时点（小时列表，如 [9,21]），默认 [9]；
	// 多时点 = 失败重试窗口（claim 幂等，不会重复领）
	RefreshHours   []int         // token 预刷新小时，默认 [3]
	JitterMinutes  int           // 签到随机延迟窗口（分钟），默认 60；负数 = 关闭
	BalanceRefresh time.Duration // 余额/过期数据后台刷新间隔，默认 30m；0 = 关闭
	RefreshSkew    time.Duration // 预刷新窗口，默认 24h

	// LogPath 签到日志落盘路径（data/checkin.jsonl）；空 = 仅内存。
	LogPath string

	// Notifier 签到结果 webhook（nil = 不推送）。
	Notifier *Notifier
	// NotifyMode 推送策略：fail（默认，有失败/未执行才推）/ always（每个窗口都推）/ never。
	NotifyMode string
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config
	log *CheckinLog
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9}
	}
	if len(cfg.RefreshHours) == 0 {
		cfg.RefreshHours = []int{3}
	}
	if cfg.JitterMinutes == 0 {
		cfg.JitterMinutes = 60
	}
	if cfg.JitterMinutes < 0 {
		cfg.JitterMinutes = 0
	}
	if cfg.BalanceRefresh < 0 {
		cfg.BalanceRefresh = 0
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 24 * time.Hour
	}
	if cfg.NotifyMode == "" {
		cfg.NotifyMode = "fail"
	}
	return &Scheduler{cfg: cfg, log: NewCheckinLog(cfg.LogPath)}
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	// 余额/过期数据后台刷新（让「积分先过期优先」路由的数据保持新鲜）。
	if s.cfg.BalanceRefresh > 0 {
		go func() {
			t := time.NewTicker(s.cfg.BalanceRefresh)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					s.refreshBalances()
				}
			}
		}()
	}
	// 启动补签：容器在任一签到窗口内重启过，则为各账号安排带抖动的补签
	// （CheckinUID 幂等：上游已签到则直接返回 already）。
	if s.inCheckinWindow(time.Now()) {
		log.Printf("startup within checkin window — scheduling jittered checkins")
		s.scheduleCheckins(ctx)
		s.scheduleDigest(ctx)
	}
	all := append(append([]int{}, s.cfg.RefreshHours...), s.cfg.CheckinHours...)
	for {
		next := nextFire(time.Now(), all)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			h := time.Now().Hour()
			if contains(s.cfg.RefreshHours, h) {
				s.RunRefreshNow()
			}
			if contains(s.cfg.CheckinHours, h) {
				s.scheduleCheckins(ctx)
				s.scheduleDigest(ctx)
			}
		}
	}
}

// inCheckinWindow 报告 now 是否落在任一签到时点的 [H:00, H+Jitter) 内。
func (s *Scheduler) inCheckinWindow(now time.Time) bool {
	for _, h := range s.cfg.CheckinHours {
		start := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		end := start.Add(time.Duration(s.cfg.JitterMinutes) * time.Minute)
		if !now.Before(start) && now.Before(end) {
			return true
		}
	}
	return false
}

// scheduleCheckins 为每个账号安排一次带独立随机延迟的签到（每日重掷，账号间互不相同）。
// 立即返回；各 goroutine 自行等待后执行（CheckinUID 幂等）。
func (s *Scheduler) scheduleCheckins(ctx context.Context) {
	window := time.Duration(s.cfg.JitterMinutes) * time.Minute
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		d := jitterDuration(window)
		go func(uid string, d time.Duration) {
			if d > 0 {
				t := time.NewTimer(d)
				defer t.Stop()
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
			log.Printf("checkin (jittered) uid=%s firing after %s", uid, d)
			s.CheckinUID(uid)
		}(st.UID, d)
	}
}

// jitterDuration 在 [0, window) 内取随机时长；window<=0 返回 0。
func jitterDuration(window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(window)))
}

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// Result 单账号运维操作结果（签到 / token 刷新），供 /admin 手动触发接口聚合返回。
type Result struct {
	UID    string `json:"uid"`
	Status string `json:"status"` // claimed / already / checkin_off / refreshed / skipped / error
	Error  string `json:"error,omitempty"`
}

// CheckinUID 对单个账号执行签到 + 积分刷新 + 解冻（定时自动路径）。
// 幂等（先查状态再领取），结果写入签到日志。
func (s *Scheduler) CheckinUID(uid string) Result { return s.checkinUID(uid, false) }

// CheckinUIDManual 面板手动触发（仅影响日志里的 manual 标记）。
func (s *Scheduler) CheckinUIDManual(uid string) Result { return s.checkinUID(uid, true) }

func (s *Scheduler) checkinUID(uid string, manual bool) Result {
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil || a.RefreshTokenValue() == "" {
		res := Result{UID: uid, Status: "skipped", Error: "no auth"}
		s.log.Add(CheckinLogEntry{TS: time.Now().Unix(), UID: uid, Status: res.Status, Error: res.Error, Manual: manual})
		return res
	}
	res := Result{UID: uid}
	// 签到（status → 未签到则 claim）
	checkedIn, _, enable, err := s.cfg.Upstream.CheckinStatus(a)
	switch {
	case err != nil:
		res.Status = "error"
		res.Error = err.Error()
		log.Printf("checkin status %s: %v", uid, err)
	case checkedIn:
		res.Status = "already"
		log.Printf("checkin %s: already checked in", uid)
	case !enable:
		res.Status = "checkin_off"
		log.Printf("checkin %s: checkin disabled upstream", uid)
	default:
		// 9074「当前参与用户太多」实测为确定性反滥用拒绝（随机设备号未注册），
		// 重放无意义——单次失败即返回，让错误原样暴露给面板。
		// 9095「当前设备今日已经签到」：共享注册设备号（TW2A_UG_DEVICE_ID）时
		// 当日名额已被占，语义等同已签到，不算错误。
		if claimErr := s.cfg.Upstream.CheckinClaim(a); claimErr != nil {
			if strings.Contains(claimErr.Error(), "9095") {
				res.Status = "already"
				log.Printf("checkin %s: device already claimed today (9095)", uid)
			} else {
				res.Status = "error"
				res.Error = claimErr.Error()
				if strings.Contains(claimErr.Error(), "9074") {
					res.Error += " —— 该账号的设备号未在上游注册：点账号行的「设备」按钮重新注册一台独立设备即可"
				}
				log.Printf("checkin claim %s: %v", uid, claimErr)
			}
		} else {
			res.Status = "claimed"
			log.Printf("checkin %s: ok", uid)
		}
	}
	// 查积分 + 解冻（无论签到结果，冷却账号按最新积分判断解冻），
	// 同时把「最近过期包」写回池，供选号「积分先过期优先」使用。
	if packs, remain, _, _, err := s.cfg.Upstream.EntUsageDetail(a); err != nil {
		log.Printf("ent-usage %s: %v", uid, err)
	} else {
		s.cfg.Pool.SetExpiry(uid, upstream.SoonestExpiry(packs))
		s.cfg.Pool.ReenableIfCredits(uid, remain)
	}
	s.cfg.Pool.SetCheckin(uid, res.Status)
	s.log.Add(CheckinLogEntry{
		TS: time.Now().Unix(), UID: uid, Nickname: a.Nickname,
		Status: res.Status, Error: res.Error, Manual: manual,
	})
	return res
}

// RecentCheckins 最近 n 条签到记录（最新在前），供面板「自动签到」看板。
func (s *Scheduler) RecentCheckins(n int) []CheckinLogEntry { return s.log.Recent(n) }

// PruneCheckins 按保留期裁剪签到日志（keepDays：<0 不动作 / 0 清空 / >0 保留最近 N 天）。
func (s *Scheduler) PruneCheckins(keepDays int) (removed, kept int, err error) {
	return s.log.Prune(keepDays)
}

// CheckinLogStats 签到日志概况（条数 + 最早/最新 unix 秒）。
func (s *Scheduler) CheckinLogStats() (entries int, oldest, newest int64) {
	return s.log.Stats()
}

// NotifyConfigured 是否配置了 webhook 推送。
func (s *Scheduler) NotifyConfigured() bool { return s.cfg.Notifier != nil }

// NotifyModeName 返回推送策略名（fail/always/never）。
func (s *Scheduler) NotifyModeName() string { return s.cfg.NotifyMode }

// CheckinLogPath 签到日志落盘路径。
func (s *Scheduler) CheckinLogPath() string { return s.cfg.LogPath }

// CheckinsSince 返回 since（unix 秒）之后的签到记录。
func (s *Scheduler) CheckinsSince(since int64) []CheckinLogEntry { return s.log.Since(since) }

// CheckinHours 返回配置的签到时点（本地小时）。
func (s *Scheduler) CheckinHours() []int { return append([]int{}, s.cfg.CheckinHours...) }

// JitterMinutes 返回签到随机延迟窗口（分钟）。
func (s *Scheduler) JitterMinutes() int { return s.cfg.JitterMinutes }

// NextCheckinWindow 返回下一次签到时点与抖动窗口结束时刻（本地时间）。
func (s *Scheduler) NextCheckinWindow() (time.Time, time.Time) {
	start := nextFire(time.Now(), s.cfg.CheckinHours)
	return start, start.Add(time.Duration(s.cfg.JitterMinutes) * time.Minute)
}

// scheduleDigest 在抖动窗口收尾后推送一次今日汇总（按天一条，避免每条签到都推）。
func (s *Scheduler) scheduleDigest(ctx context.Context) {
	if s.cfg.Notifier == nil {
		return
	}
	d := time.Duration(s.cfg.JitterMinutes)*time.Minute + 2*time.Minute
	go func() {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.notifyDigest("（定时窗口）")
		}
	}()
}

// TodayDigest 汇总「今天到目前」的签到结果（按账号取当天最后一条）。
// 返回给人类看的文本与"是否存在需要关注的问题"。
func (s *Scheduler) TodayDigest() (string, bool) {
	since := StartOfToday(time.Now())
	latest := map[string]CheckinLogEntry{}
	for _, e := range s.log.Since(since) {
		latest[e.UID] = e
	}
	total, claimed, already, errN, off, noRec := 0, 0, 0, 0, 0, 0
	var problems []string
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		total++
		name := st.Nickname
		if name == "" {
			name = st.UID
			if len(name) > 8 {
				name = name[:8] + "…"
			}
		}
		e, ok := latest[st.UID]
		if !ok {
			noRec++
			problems = append(problems, name+"：今日无签到记录")
			continue
		}
		switch e.Status {
		case "claimed":
			claimed++
		case "already":
			already++
		case "error":
			errN++
			problems = append(problems, name+"：失败 "+oneLine(e.Error))
		case "checkin_off":
			off++
		}
	}
	body := fmt.Sprintf("账号 %d · 已签 %d（领取 %d + 今日已签 %d）· 失败 %d · 未执行 %d",
		total, claimed+already, claimed, already, errN, noRec)
	if off > 0 {
		body += fmt.Sprintf(" · 上游未开放 %d", off)
	}
	if len(problems) > 0 {
		body += "\n" + strings.Join(problems, "\n")
	}
	text := fmt.Sprintf("trae2api 签到 %s\n%s", time.Now().Format("01-02 15:04"), body)
	return text, errN+noRec > 0
}

// notifyDigest 按策略推送到 webhook（未配置则什么都不做）。
func (s *Scheduler) notifyDigest(trigger string) {
	if s.cfg.Notifier == nil {
		return
	}
	if s.cfg.NotifyMode == "never" {
		return
	}
	text, hasFail := s.TodayDigest()
	if s.cfg.NotifyMode != "always" && !hasFail {
		return
	}
	if err := s.cfg.Notifier.Send("trae2api 签到"+trigger, text); err != nil {
		log.Printf("checkin notify failed (kind=%s): %v", s.cfg.Notifier.Kind(), err)
		return
	}
	log.Printf("checkin notify sent (%s, kind=%s, has_fail=%v)", trigger, s.cfg.Notifier.Kind(), hasFail)
}

// oneLine 把错误压成一行短文本。
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// RunCheckinNow 立即对所有账号执行签到 + 积分刷新 + 解冻，返回逐号结果。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
func (s *Scheduler) RunCheckinNow() []Result { return s.runCheckinNow(false) }

// RunCheckinNowManual 面板「全部签到」（日志标记 manual）。
func (s *Scheduler) RunCheckinNowManual() []Result { return s.runCheckinNow(true) }

func (s *Scheduler) runCheckinNow(manual bool) []Result {
	out := make([]Result, 0)
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		out = append(out, s.checkinUID(st.UID, manual))
	}
	if manual {
		s.notifyDigest("（手动全部签到）")
	}
	return out
}

// refreshBalances 后台静默刷新全部账号的积分与最近过期包
// （喂给「积分先过期优先」选号与面板额度展示；不写日志除非出错）。
func (s *Scheduler) refreshBalances() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		packs, remain, _, _, err := s.cfg.Upstream.EntUsageDetail(a)
		if err != nil {
			continue
		}
		s.cfg.Pool.SetExpiry(st.UID, upstream.SoonestExpiry(packs))
		s.cfg.Pool.SetCredits(st.UID, remain)
	}
}

// RefreshUID 刷新单账号 token 并落盘；session 失效自动禁用。
// 供 RunRefreshNow 循环与 /admin 手动刷新共用。
func (s *Scheduler) RefreshUID(uid string) Result {
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil || a.RefreshTokenValue() == "" {
		return Result{UID: uid, Status: "skipped", Error: "no auth"}
	}
	if err := s.cfg.Upstream.RefreshToken(a); err != nil {
		log.Printf("refresh %s: %v", uid, err)
		var ue *upstream.Error
		if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
			s.cfg.Pool.Disable(uid, "session dead")
		}
		return Result{UID: uid, Status: "error", Error: err.Error()}
	}
	if err := a.SaveAtomic(); err != nil {
		log.Printf("refresh %s save: %v", uid, err)
		return Result{UID: uid, Status: "refreshed", Error: "save: " + err.Error()}
	}
	return Result{UID: uid, Status: "refreshed"}
}

// RunRefreshNow 立即对所有账号刷新 token（尊重 NeedsRefresh 预刷新窗口，
// 未到窗口的记 skipped），返回逐号结果。
func (s *Scheduler) RunRefreshNow() []Result {
	out := make([]Result, 0)
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if !a.NeedsRefresh(s.cfg.RefreshSkew) {
			out = append(out, Result{UID: st.UID, Status: "skipped"})
			continue
		}
		out = append(out, s.RefreshUID(st.UID))
	}
	return out
}
