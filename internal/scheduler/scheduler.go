// Package scheduler 定时任务：每日签到 + token 预刷新。
// 签到成功后重新查积分，积分 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"trae2api-web/internal/pool"
	"trae2api-web/internal/upstream"
)

// Config 调度器依赖。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	CheckinHour  int           // 每日签到小时，默认 9
	RefreshHours []int         // token 预刷新小时，默认 [3]
	RefreshSkew  time.Duration // 预刷新窗口，默认 24h
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
				s.RunCheckinNow()
			}
		}
	}
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
		// 9074 是「设备号无效」与「高峰限流」共用的码：短间隔重试 2 次。
		// 设备无效时重试快速失败无害；高峰限流时重试大概率命中（上游有 checkin_retry 先例）。
		for attempt := 0; ; attempt++ {
			if claimErr := s.cfg.Upstream.CheckinClaim(a); claimErr != nil {
				res.Status = "error"
				res.Error = claimErr.Error()
				log.Printf("checkin claim %s: %v", uid, claimErr)
				if attempt < 2 && strings.Contains(claimErr.Error(), "9074") {
					time.Sleep(2 * time.Second)
					continue
				}
			} else {
				res.Status = "claimed"
				log.Printf("checkin %s: ok", uid)
			}
			break
		}
	}
	// 查积分 + 解冻（无论签到结果，冷却账号按最新积分判断解冻）
	if remain, err := s.cfg.Upstream.UserEntUsage(a); err != nil {
		log.Printf("ent-usage %s: %v", uid, err)
	} else {
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
