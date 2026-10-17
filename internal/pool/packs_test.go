// packs_test.go 积分包快照行为锁：
//
//  1. SetPacks 覆盖快照并重算最早过期时刻（remaining>0 的包）；
//  2. 过期超 48h 的旧包自动丢弃（节省 state.json），48h 内保留（供面板展示）；
//  3. state.json 往返（load → packs/expireAt 保留）。
package pool

import (
	"path/filepath"
	"testing"
	"time"

	"trae2api-web/internal/auth"
)

func TestPackSnapshotLifecycle(t *testing.T) {
	p := New(t.TempDir() + string(filepath.Separator) + "state.json")
	p.Add(&auth.Auth{UID: "u1", Nickname: "n1"})
	if _, ok := p.byUID["u1"]; !ok {
		t.Fatal("add failed")
	}
	now := time.Now().Unix()
	p.SetPacks("u1", []PackInfo{
		{Group: "每日签到", Name: "签到奖励", Limit: 100, Used: 30, ExpireAt: now + 3600},
		{Group: "周期", Name: "老包", Limit: 50, Used: 0, ExpireAt: now - 72*3600}, // 3 天前 → 丢弃
	})
	ps := p.PacksOf("u1")
	if len(ps) != 1 || ps[0].Remain() != 70 {
		t.Fatalf("packs=%+v", ps)
	}
	l := p.List()
	if l[0].ExpireAt != now+3600 || len(l[0].Packs) != 1 {
		t.Fatalf("list=%+v", l[0])
	}

	// 全空包：expireAt 归零（快照为空 → 选号视为未知）
	p.SetPacks("u1", nil)
	if ep := p.PacksOf("u1"); len(ep) != 0 {
		t.Fatalf("empty packs kept: %+v", ep)
	}
	if l = p.List(); l[0].ExpireAt != 0 {
		t.Fatalf("expire_at=%d want 0", l[0].ExpireAt)
	}
}

func TestPackSnapshotPersistAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := dir + string(filepath.Separator) + "state.json"
	p := New(fp)
	p.Add(&auth.Auth{UID: "k", Nickname: "kk"})
	fut := time.Now().Add(24 * time.Hour).Unix()
	p.SetPacks("k", []PackInfo{{Group: "x", Limit: 10, Used: 2, ExpireAt: fut}})
	p.SetCredits("k", 8) // 触发 saveLocked 落盘

	p2 := New(fp)
	if got := p2.PacksOf("k"); len(got) != 1 || got[0].Remain() != 8 || got[0].ExpireAt != fut {
		t.Fatalf("reload packs=%+v", got)
	}
}
