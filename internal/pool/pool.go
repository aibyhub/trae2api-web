// Package pool 账号池：内存索引 + 冷却/禁用状态机 + state.json 持久化。
// 挑选策略：healthy 账号中积分最近过期日优先（用掉快过期的额度），
// 同过期日比「近期使用降权后」的剩余积分（把流量摊开）；过期信息未知时退化为积分最多者。
// 积分变化会同步记入 data/credits.jsonl（余额轨迹，面板图表用）。
package pool

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"trae2api-web/internal/auth"
	"trae2api-web/internal/upstream"
)

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolPlan CoolKind = iota // 1005 plan 权益不足 → 12h 长冷却
	CoolSoft                 // 429/404 → 60s 短冷却（404 不累计 errCount）
	CoolErr                  // 连续错误 → 10m 中冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolPlan:
		return "plan_limit"
	case CoolSoft:
		return "soft_rate"
	case CoolErr:
		return "error_threshold"
	}
	return "unknown"
}

// Status 单个账号对外暴露的状态（脱敏，不含 token）。
type Status struct {
	UID      string    `json:"uid"`
	Nickname string    `json:"nickname,omitempty"`
	Credits  int64     `json:"credits"`
	Cooling  bool      `json:"cooling"`
	Until    time.Time `json:"until,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	// Disabled = session 失效硬禁用（需重登或换文件恢复）；Enabled = 软开关（用户可逆启停）。
	// 对外暴露：Disabled 与 Enabled 都为 false 才算可被 Pick（healthy）。
	Disabled bool  `json:"disabled"`
	Enabled  bool  `json:"enabled"`
	ErrCount int   `json:"err_count,omitempty"`
	ExpireAt int64 `json:"expire_at,omitempty"` // 最近一份仍有余量的权益包过期时刻（unix 秒）；0 = 未知

	// TransportDown = 该账号出口（代理/网络）处于临时隔离窗口（见 NoteTransportError）。
	// 与账号健康无关：仅表示"现在别用这个出口"，不累计 errCount、不写 state.json。
	TransportDown     bool       `json:"transport_down,omitempty"`
	TransportReason   string     `json:"transport_reason,omitempty"`
	TransportUntil    time.Time  `json:"transport_until,omitempty"`
	LastCheckinAt     int64      `json:"last_checkin_at,omitempty"`     // 最近一次签到结果时间（unix 秒）
	LastCheckinStatus string     `json:"last_checkin_status,omitempty"` // claimed/already/checkin_off/error...
	Packs             []PackInfo `json:"packs,omitempty"`               // 最近一次刷到的积分包快照（到期日历用）
}

// PackInfo 单个积分包快照（scheduler 的 EntUsageDetail 回填；不含空包）。
type PackInfo struct {
	Group    string `json:"group"`          // 分组，如「每日签到」
	Name     string `json:"name,omitempty"` // display_desc
	Limit    int64  `json:"limit"`
	Used     int64  `json:"used"`
	ExpireAt int64  `json:"expire_at,omitempty"` // unix 秒；0 = 未知
}

// Remain 该包剩余积分（limit-used）。
func (p PackInfo) Remain() int64 { return p.Limit - p.Used }

type entry struct {
	a        *auth.Auth
	credits  int64
	disabled bool // session dead 硬禁用
	enabled  bool // 用户软开关（默认 true），false 时 Pick 跳过
	reason   string
	until    time.Time
	errCount int
	expireAt int64 // 最近过期包（unix 秒）；0 = 未知
	// 出口/网络临时隔离（内存态，不落盘）：代理不可达、连接被重置等导致的一次失败。
	transportUntil  time.Time
	transportReason string
	lastCheckinAt   int64       // 最近签到时间（unix 秒）
	lastCheckin     string      // 最近签到结果
	packs           []PackInfo  // 最近一次 EntUsageDetail 快照（内存 + 落盘）
	pickStamps      []time.Time // 近 1h 成功调用节拍（内存态）：同过期日积分对比时按此降权
}

// 选号均衡参数：同过期日的账号比较「近窗降权积分」，公平除数 = 1 + decay × 近 1h
// 成功笔数（笔数封顶 cap），近期多用者让位，把流量摊到同期到期包之间。
const (
	fairnessWindow = time.Hour
	fairnessCap    = 16
	fairnessDecay  = 0.25
)

func (e *entry) healthy(now time.Time) bool {
	if e.disabled || !e.enabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	// 出口故障临时隔离：不惩罚账号，但窗口内不参与选号（避免反复选中同一个坏出口）。
	if !e.transportUntil.IsZero() && now.Before(e.transportUntil) {
		return false
	}
	return true
}

// stateEntry state.json 单账号持久化条目。
type stateEntry struct {
	Credits           int64      `json:"credits"`
	Disabled          bool       `json:"disabled"`
	Enabled           *bool      `json:"enabled,omitempty"` // 指针：旧文件缺省时按 true 处理，不写回脏值
	Reason            string     `json:"reason,omitempty"`
	Until             time.Time  `json:"until,omitempty"`
	ExpireAt          int64      `json:"expire_at,omitempty"` // 最近过期包（unix 秒）
	LastCheckinAt     int64      `json:"last_checkin_at,omitempty"`
	LastCheckinStatus string     `json:"last_checkin_status,omitempty"`
	Packs             []PackInfo `json:"packs,omitempty"` // 积分包快照（到期日历）
}

// stateFile 持久化格式。
type stateFile struct {
	Accounts map[string]stateEntry `json:"accounts"`
}

// Pool 账号池。
type Pool struct {
	mu       sync.RWMutex
	byUID    map[string]*entry
	stateFp  string
	creditFp string // credits.jsonl（积分余额轨迹）；空 = 不记录
}

// New 构建池；stateFp 非空时尝试加载旧状态，并把积分轨迹定在 stateFp 同目录（data/credits.jsonl）。
func New(stateFp string) *Pool {
	p := &Pool{byUID: map[string]*entry{}, stateFp: stateFp}
	if stateFp != "" {
		p.load()
		if dir := filepath.Dir(stateFp); dir != "" {
			p.creditFp = filepath.Join(dir, "credits.jsonl")
		}
	}
	return p
}

// Add 加入账号；已存在则保留原状态、更新凭证。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling/enabled 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a, enabled: true}
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range auths {
		seen[a.UID] = true
		if e, ok := p.byUID[a.UID]; ok {
			e.a = a
		} else {
			p.byUID[a.UID] = &entry{a: a, enabled: true}
		}
	}
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
		}
	}
}

// Remove 删除账号（仅清内存索引与 state.json 条目；auths/trae-{uid}.json 由调用方删）。
// 不存在返回 false，调用方据此决定 404。
func (p *Pool) Remove(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.byUID[uid]; !ok {
		return false
	}
	delete(p.byUID, uid)
	p.saveLocked()
	return true
}

// SetEnabled 切换账号软开关；不影响 disabled（session dead）状态。
// reason 仅在关闭时记录。不存在返回 false。
func (p *Pool) SetEnabled(uid string, enabled bool, reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.enabled = enabled
	if !enabled && reason != "" {
		e.reason = reason
	}
	if enabled {
		// 重新启用时清掉软关闭的 reason；disabled/cooling 不动
		if e.reason != "" && !e.disabled && e.until.IsZero() {
			e.reason = ""
		}
	}
	p.saveLocked()
	return true
}

// Pick 返回 healthy 中积分最高的账号；无可用返回 nil。
func (p *Pool) Pick() *auth.Auth {
	return p.PickExcluding(nil)
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
// 选号优先级：积分最近过期日优先（用过期前的额度），同日比近期使用降权后的积分；
// expireAt 未知（0）视为最晚，排在使用已知过期信息的账号之后。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if best == nil || expireFirst(e, best, now) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	return best.a
}

// expireFirst 报告 a 是否应排在 b 之前（先用掉快过期的额度，同时把流量摊开）。
// 主序：最近过期日优先 —— 按 UTC 日分桶比较（按秒精确比会把 tie-break 架空：
// 两个同日过期的账号只差几分几秒时，旧逻辑永远固定粘住稍早几秒的那个，长期忽略其他号）。
// 同桶：比「近 1h 使用」降权后的积分（近期多用者让位）；降权后仍并列退回原始积分保证确定性。
func expireFirst(a, b *entry, now time.Time) bool {
	ea, eb := expireBucket(a.expireAt), expireBucket(b.expireAt)
	if ea != eb {
		return ea < eb
	}
	if fa, fb := fairCredits(a, now), fairCredits(b, now); fa != fb {
		return fa > fb
	}
	return a.credits > b.credits
}

// expireBucket 过期时刻按 UTC 日分桶（86400s/桶）；0（未知）视为最晚。
func expireBucket(expireAt int64) int64 {
	if expireAt <= 0 {
		return math.MaxInt64
	}
	return expireAt / 86400
}

// fairCredits 近窗使用降权后的积分：credits / (1 + decay × 近 1h 成功笔数)，笔数封顶。
func fairCredits(e *entry, now time.Time) float64 {
	cutoff := now.Add(-fairnessWindow)
	n := 0
	for _, ts := range e.pickStamps {
		if ts.After(cutoff) {
			n++
		}
	}
	if n > fairnessCap {
		n = fairnessCap
	}
	return float64(e.credits) / (1 + fairnessDecay*float64(n))
}

// notePickLocked 记一笔近期成功调用节拍，并修剪窗外与超额头寸。
func (e *entry) notePickLocked(now time.Time) {
	e.pickStamps = append(e.pickStamps, now)
	cutoff := now.Add(-fairnessWindow)
	i := 0
	for i < len(e.pickStamps) && !e.pickStamps[i].After(cutoff) {
		i++
	}
	e.pickStamps = e.pickStamps[i:]
	if len(e.pickStamps) > fairnessCap {
		e.pickStamps = e.pickStamps[len(e.pickStamps)-fairnessCap:]
	}
}

// SetExpiry 记录账号最近一份仍有余量的权益包过期时刻（unix 秒；0 = 未知）。
// 数据源：scheduler 每日签到后的 EntUsageDetail 与面板额度刷新。
func (p *Pool) SetExpiry(uid string, expireAt int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.expireAt = expireAt
	}
}

// SetPacks 覆盖账号的积分包快照（scheduler EntUsageDetail 成功后调用）。
// 同时从快照重算最早到期时刻；已过包保留 48h（面板"已过期 N 天"展示），其后丢弃。
func (p *Pool) SetPacks(uid string, packs []PackInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	now := time.Now().Unix()
	kept := packs[:0:0]
	soonest := int64(0)
	for _, pk := range packs {
		if pk.Limit <= 0 {
			continue
		}
		if pk.ExpireAt > 0 && pk.ExpireAt < now-48*3600 {
			continue // 过期超过 48h 的旧包快照
		}
		kept = append(kept, pk)
		if pk.Remain() > 0 && pk.ExpireAt > 0 && (soonest == 0 || pk.ExpireAt < soonest) {
			soonest = pk.ExpireAt
		}
	}
	e.packs = kept
	// 无任何「仍有余量且有到期日」的包 → expireAt 归零（未知，退回积分排序）。
	// 2026-10-25 修复：旧条件 `soonest > 0 || len(packs) == 0` 在「输入非空但全部
	// 过期超 48h」时保留陈旧 expireAt，expireBucket 会把该账号判为最早过期
	// 永远优先选中 → 1005 → 冷却 → 再选中的死循环，挤掉健康账号。
	if soonest > 0 || len(kept) == 0 {
		e.expireAt = soonest
	}
}

// PacksFromEnt upstream.EntPack → PackInfo（快照只留展示所需字段；scheduler/管理端共用）。
func PacksFromEnt(ps []upstream.EntPack) []PackInfo {
	out := make([]PackInfo, 0, len(ps))
	for _, p := range ps {
		out = append(out, PackInfo{
			Group: p.Group, Name: p.Name,
			Limit: p.Limit, Used: p.Used, ExpireAt: p.ExpireAt,
		})
	}
	return out
}

// PacksOf 账号的积分包快照（面板到期日历）；无快照返回 nil。
func (p *Pool) PacksOf(uid string) []PackInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.packs
	}
	return nil
}

// SetCredits 更新账号积分（有变化时记一条 credits.jsonl 轨迹）。
func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	p.setCreditsLocked(uid, credits, "refresh")
	p.saveLocked()
	p.mu.Unlock()
}

// setCreditsLocked 更新积分；有变化时把变更记进 credits.jsonl。需持锁。
func (p *Pool) setCreditsLocked(uid string, credits int64, source string) {
	if e, ok := p.byUID[uid]; ok {
		if e.credits != credits {
			p.appendCreditLocked(creditLine{
				TS:      time.Now().Unix(),
				UID:     uid,
				Credits: credits,
				Delta:   credits - e.credits,
				Source:  source,
			})
		}
		e.credits = credits
	}
}

// Cooldown 冷却账号至 now+d。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.reason = reason
		e.errCount = 0
	}
	p.saveLocked()
}

// Disable 永久禁用（session 失效），需人工重登后手工恢复或文件替换。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
	}
	p.saveLocked()
}

// ReenableIfCredits 签到后解冻：仅当 remain > 0 且账号处于冷却（非禁用）时恢复。
// 积分变化记 credits.jsonl（source=checkin）。
func (p *Pool) ReenableIfCredits(uid string, remain int64) {
	p.mu.Lock()
	p.setCreditsLocked(uid, remain, "checkin")
	if e, ok := p.byUID[uid]; ok {
		if remain > 0 && !e.disabled {
			e.until = time.Time{}
			e.reason = ""
			e.errCount = 0
		}
	}
	p.saveLocked()
	p.mu.Unlock()
}

// NoteError 记录一次错误；达到 threshold 自动冷却 d 时长。
func (p *Pool) NoteError(uid string, threshold int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount++
		if e.errCount >= threshold {
			e.until = time.Now().Add(d)
			e.reason = "consecutive errors"
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// NoteSuccess 成功请求重置错误计数、清除出口故障隔离，并记一笔近期成功节拍
// （同过期日积分对比的降权依据）。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount = 0
		e.transportUntil = time.Time{}
		e.transportReason = ""
		e.notePickLocked(time.Now())
	}
}

// NoteTransportError 记录一次「出口/网络故障」（代理不可达、连接被重置、畸形响应、超时…）。
// 与账号健康无关：不累计 errCount、不改 disabled/cooling，仅在 d 窗口内把该账号从选号中
// 摘出去，避免同一个坏出口被反复选中；任一成功请求立即解除（NoteSuccess）。
// 纯内存态：不写 state.json，进程重启后重新探测。
func (p *Pool) NoteTransportError(uid string, d time.Duration, reason string) {
	if d <= 0 {
		d = 5 * time.Minute
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.transportUntil = time.Now().Add(d)
		e.transportReason = reason
	}
}

// SetCheckin 记录最近一次签到结果（面板展示：一眼看出哪个账号今天没签/失败）。
func (p *Pool) SetCheckin(uid, status string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.lastCheckinAt = time.Now().Unix()
		e.lastCheckin = status
	}
	p.saveLocked()
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}

func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	return Status{
		UID:      uid,
		Nickname: e.a.Nickname,
		Credits:  e.credits,
		Cooling:  !e.until.IsZero() && now.Before(e.until),
		Until:    e.until,
		Reason:   e.reason,
		Disabled: e.disabled,
		Enabled:  e.enabled,
		ErrCount: e.errCount,
		ExpireAt: e.expireAt,

		TransportDown:     !e.transportUntil.IsZero() && now.Before(e.transportUntil),
		TransportReason:   e.transportReason,
		TransportUntil:    e.transportUntil,
		LastCheckinAt:     e.lastCheckinAt,
		LastCheckinStatus: e.lastCheckin,
		Packs:             e.packs,
	}
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	for uid, s := range sf.Accounts {
		enabled := true // 旧文件无 enabled 字段 → 默认启用，向后兼容
		if s.Enabled != nil {
			enabled = *s.Enabled
		}
		p.byUID[uid] = &entry{
			a:        &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:  s.Credits,
			disabled: s.Disabled,
			enabled:  enabled,
			reason:   s.Reason,
			until:    s.Until,
			expireAt: s.ExpireAt,
			packs:    s.Packs,

			lastCheckinAt: s.LastCheckinAt,
			lastCheckin:   s.LastCheckinStatus,
		}
	}
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{Accounts: map[string]stateEntry{}}
	for uid, e := range p.byUID {
		now := time.Now().Unix()
		kept := e.packs[:0:0]
		for _, pk := range e.packs {
			if pk.ExpireAt > 0 && pk.ExpireAt < now-48*3600 {
				continue
			}
			kept = append(kept, pk)
		}
		e.packs = kept
		se := stateEntry{
			Credits:  e.credits,
			Disabled: e.disabled,
			Reason:   e.reason,
			Until:    e.until,
			ExpireAt: e.expireAt,
			Packs:    kept,

			LastCheckinAt:     e.lastCheckinAt,
			LastCheckinStatus: e.lastCheckin,
		}
		// 仅在软关闭时写 enabled=false；默认 true 用 omitempty 省略，旧版本读为 true。
		if !e.enabled {
			f := false
			se.Enabled = &f
		}
		sf.Accounts[uid] = se
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.stateFp)
}
