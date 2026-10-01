// transport_test.go 出口故障隔离与最近签到记录（v1.2.9）。
package pool

import (
	"path/filepath"
	"testing"
	"time"

	"trae2api-web/internal/auth"
)

// TestTransportErrorIsolatesAccountWithoutPenalty 出口/网络故障只临时隔离，不惩罚账号。
func TestTransportErrorIsolatesAccountWithoutPenalty(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 10)

	// u1 积分更高本该被选中；记录一次出口故障后应被临时隔离。
	p.NoteTransportError("u1", time.Minute, "proxy down")
	st, ok := p.Status("u1")
	if !ok || !st.TransportDown {
		t.Fatalf("status=%+v want transport_down", st)
	}
	if st.ErrCount != 0 || st.Cooling || st.Disabled {
		t.Fatalf("transport error must not penalize the account: %+v", st)
	}
	if got := p.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("pick=%v want u2 (u1 isolated)", got)
	}

	// 一次成功即解除隔离。
	p.NoteSuccess("u1")
	if st, _ := p.Status("u1"); st.TransportDown {
		t.Fatal("NoteSuccess must clear the transport isolation")
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("pick=%v want u1 after recovery", got)
	}
}

// TestSetCheckinPersisted 最近签到结果落盘（面板展示），出口隔离不落盘。
func TestSetCheckinPersisted(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteTransportError("u1", time.Minute, "proxy down")
	p.SetCheckin("u1", "claimed")

	p2 := New(fp)
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("u1 missing after reload")
	}
	if st.LastCheckinStatus != "claimed" || st.LastCheckinAt == 0 {
		t.Fatalf("last checkin not persisted: %+v", st)
	}
	if st.TransportDown {
		t.Fatal("transport isolation is in-memory only and must not survive a restart")
	}
}
