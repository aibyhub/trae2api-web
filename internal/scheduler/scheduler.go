// Package scheduler 定时任务：每日签到 + token 预刷新。
// 签到成功后重新查积分，积分 > 0 的冷却账号自动解冻。
// 签到时刻在 CheckinHour 后按每账号独立的随机 0~JitterMinutes 延迟执行
// （每日重掷、账号间互不相同），让上游看到的时间分布更像人操作。
package scheduler

import (
	"context"
	"errors"
	"log"
	"math/rand/v2"
	"strings"
	"time"

	"trae2api-web/internal/pool"
	"trae2api-web/internal/upstream"
)

// Config 调度器依赖。
type Config struct {
	Pool          *pool.Pool
	Upstream      *upstream.Client
	CheckinHour   int           // 每日签到小时，默认 9
	RefreshHours  []int         // token 预刷新小时，默认 [3]
	JitterMinutes int           // 签到随机延迟窗口（分钟），默认 60；负数 = 关闭
	RefreshSkew   time.Duration // 预刷新窗口，默认 24h
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config
}

// New 构建。
func New(cfg Config) *Scheduler {
	if cfg.CheckinHour < 0 {
		cfg.CheckinHour = 9
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
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 24 * time.Hour
	}
	return &Scheduler{cfg: cfg}
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
	// 启动补签：容器在签到窗口内重启过，则为各账号安排带抖动的补签
	// （CheckinUID 幂等：上游已签到则直接返回 already）。
	if s.inCheckinWindow(time.Now()) {
		log.Printf("startup within checkin window — scheduling jittered checkins")
		s.scheduleCheckins(ctx)
	}
	all := append(append([]int{}, s.cfg.RefreshHours...), s.cfg.CheckinHour)
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
			if s.cfg.CheckinHour == h {
				s.scheduleCheckins(ctx)
			}
		}
	}
}

// inCheckinWindow 报告 now 是否落在今日 [CheckinHour:00, CheckinHour+Jitter) 内。
func (s *Scheduler) inCheckinWindow(now time.Time) bool {
	start := time.Date(now.Year(), now.Month(), now.Day(), s.cfg.CheckinHour, 0, 0, 0, now.Location())
	end := start.Add(time.Duration(s.cfg.JitterMinutes) * time.Minute)
	return !now.Before(start) && now.Before(end)
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

// CheckinUID 对单个账号执行签到 + 积分刷新 + 解冻。
// 供 RunCheckinNow 循环与 /admin 手动签到共用；幂等（先查状态再领取）。
func (s *Scheduler) CheckinUID(uid string) Result {
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil || a.RefreshTokenValue() == "" {
		return Result{UID: uid, Status: "skipped", Error: "no auth"}
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
					res.Error += " —— 该账号的设备号未在上游注册：删除此账号，用面板「添加账号（TRAE 登录）」重新登录一次即可注册独立设备号"
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
	return res
}

// RunCheckinNow 立即对所有账号执行签到 + 积分刷新 + 解冻，返回逐号结果。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
func (s *Scheduler) RunCheckinNow() []Result {
	out := make([]Result, 0)
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		out = append(out, s.CheckinUID(st.UID))
	}
	return out
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
