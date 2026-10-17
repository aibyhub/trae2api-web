// fairness_test.go 选号均衡回归：
// 1) 同日过期只差 153s 的粘滞（生产实测 2398/7272 的真实事故形态）；
// 2) 近 1h 成功笔数降权把流量摊开；
// 3) 过期日主序与未知过期兜底。
package pool

import (
	"testing"
	"time"

	"trae2api-web/internal/auth"
)

func addTwo(p *Pool, uidA, uidB string, expireA, expireB, creditA, creditB int64) {
	p.Add(&auth.Auth{UID: uidA})
	p.Add(&auth.Auth{UID: uidB})
	p.SetExpiry(uidA, expireA)
	p.SetExpiry(uidB, expireB)
	p.SetCredits(uidA, creditA)
	p.SetCredits(uidB, creditB)
}

// TestFairPickSameDayExpirySpreads 两个同日过期的账号差 153s：
// 旧逻辑按秒比永远锁死早过期的那个（生产 2398 吞 44% 调用的根因）；
// 新逻辑同日桶内比「近窗降权积分」，谁近期用得多谁让位。
func TestFairPickSameDayExpirySpreads(t *testing.T) {
	p := New("")
	// base 对齐 UTC（次日 08:00 +1h），消除 base 与 base+153 跨日的翻车概率。
	base := time.Now().UTC().Truncate(24 * time.Hour).Add(33 * time.Hour).Unix()
	// 生产参数复刻：2398 早 153s、积分 4369；7272 晚 153s、积分 5497。
	addTwo(p, "2398", "7272", base+153, base, 4369, 5497)

	// 首选：同日积分大者（5497）。
	if got := p.Pick(); got == nil || got.UID != "7272" {
		t.Fatalf("first pick=%v want 7272", got)
	}

	// 7272 连续成功后窗口降权启动：除数 1+0.25×n，n=7 → 5497/2.75≈1999 < 4369 → 2398 顶上。
	for i := 0; i < 7; i++ {
		p.NoteSuccess("7272")
	}
	if got := p.Pick(); got == nil || got.UID != "2398" {
		t.Fatalf("pick=%v want 2398 after 7 recent successes on 7272", got)
	}

	// 窗口清零（等价 1h 过去）恢复原始优先级。
	p.mu.Lock()
	p.byUID["7272"].pickStamps = nil
	p.byUID["2398"].pickStamps = nil
	p.mu.Unlock()
	if got := p.Pick(); got == nil || got.UID != "7272" {
		t.Fatalf("pick=%v want 7272 after window cleared", got)
	}
}

// TestFairPickEarlierExpiryDayFirst 过期日主序：更早过期日的账号即使积分极少也优先，
// 窗口降权不得把「快过期的额度用到没」这个资源目标顶掉。
func TestFairPickEarlierExpiryDayFirst(t *testing.T) {
	p := New("")
	base := time.Now().Unix() + 2*3600
	addTwo(p, "today", "later", base, base+28*3600, 1, 99999)
	if got := p.Pick(); got == nil || got.UID != "today" {
		t.Fatalf("pick=%v want earlier expiry day", got)
	}
}

// TestFairPickWindowDoesNotCrossDay 过期日不同的两个号互不渗透：
// 近窗降权只在同日桶内生效（保证不同日的号不会互相让位）。
func TestFairPickWindowDoesNotCrossDay(t *testing.T) {
	p := New("")
	base := time.Now().Unix() + 3600
	addTwo(p, "today", "later", base, base+30*3600, 100, 5000)
	for i := 0; i < fairnessCap; i++ {
		p.NoteSuccess("today")
	}
	if got := p.Pick(); got == nil || got.UID != "today" {
		t.Fatalf("pick=%v want today (different day, no cross-day yield)", got)
	}
}

// TestFairPickUnknownExpiryLast 过期信息未知（0）视为最晚。
func TestFairPickUnknownExpiryLast(t *testing.T) {
	p := New("")
	addTwo(p, "known", "unknown", time.Now().Unix()+1800, 0, 100, 99999)
	if got := p.Pick(); got == nil || got.UID != "known" {
		t.Fatalf("pick=%v want known (unknown expiry last)", got)
	}
}

// TestFairPickCapLimit 降权笔数封顶：超过 cap 后不再加重（分子不变分母不变）。
func TestFairPickCapLimit(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "saturated"})
	for i := 0; i < fairnessCap*2; i++ {
		p.NoteSuccess("saturated")
	}
	n := 0
	cutoff := time.Now().Add(-fairnessWindow)
	for _, ts := range p.byUID["saturated"].pickStamps {
		if ts.After(cutoff) {
			n++
		}
	}
	if n != fairnessCap {
		t.Fatalf("stamps=%d want capped %d", n, fairnessCap)
	}
}
