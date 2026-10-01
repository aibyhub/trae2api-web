// notify_test.go 签到日志裁剪 + webhook 推送格式 + 今日汇总。
package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trae2api-web/internal/auth"
	"trae2api-web/internal/pool"
)

func TestCheckinLogPrune(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "checkin.jsonl")
	l := NewCheckinLog(fp)
	l.Add(CheckinLogEntry{TS: time.Now().AddDate(0, 0, -200).Unix(), UID: "old", Status: "claimed"})
	l.Add(CheckinLogEntry{TS: time.Now().Add(-time.Hour).Unix(), UID: "recent", Status: "claimed"})

	removed, kept, err := l.Prune(90)
	if err != nil || removed != 1 || kept != 1 {
		t.Fatalf("prune removed=%d kept=%d err=%v", removed, kept, err)
	}
	raw, _ := os.ReadFile(fp)
	if strings.Contains(string(raw), "old") {
		t.Fatal("过期记录必须从磁盘删除")
	}
	if !strings.Contains(string(raw), "recent") {
		t.Fatal("未过期记录必须保留")
	}
	if r, k, _ := l.Prune(0); r != 1 || k != 0 {
		t.Fatalf("clear removed=%d kept=%d", r, k)
	}
	l.Add(CheckinLogEntry{TS: time.Now().Unix(), UID: "recent2", Status: "claimed"})
	if r, k, _ := l.Prune(-1); r != 0 || k != 1 {
		t.Fatalf("负数应为不动作，得到 removed=%d kept=%d", r, k)
	}
}

func TestNotifierPayload(t *testing.T) {
	cases := []struct{ url, want string }{
		{"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=x", "msgtype"},
		{"https://oapi.dingtalk.com/robot/send?access_token=x", "msgtype"},
		{"https://open.feishu.cn/open-apis/bot/v2/hook/x", "msg_type"},
		{"https://example.com/hook", "正文"},
	}
	for _, c := range cases {
		n := NewNotifier(c.url)
		if n == nil {
			t.Fatalf("nil notifier for %s", c.url)
		}
		if got := string(n.Payload("标题", "正文")); !strings.Contains(got, c.want) {
			t.Errorf("%s payload=%s want contains %s", c.url, got, c.want)
		}
	}
	if NewNotifier("   ") != nil {
		t.Fatal("空 URL 必须返回 nil notifier")
	}
}

func TestTodayDigest(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "一号"})
	pl.Add(&auth.Auth{UID: "u2", Nickname: "二号"})
	s := New(Config{Pool: pl, LogPath: filepath.Join(t.TempDir(), "checkin.jsonl")})
	s.log.Add(CheckinLogEntry{TS: time.Now().Unix(), UID: "u1", Nickname: "一号", Status: "claimed"})

	text, hasFail := s.TodayDigest()
	if !hasFail {
		t.Fatal("u2 今日无记录 → hasFail 必须为 true")
	}
	if !strings.Contains(text, "二号") || !strings.Contains(text, "今日无签到记录") {
		t.Fatalf("digest text=%s", text)
	}
}

// TestRunCheckinNowLogsSkip 没有 refreshToken 的账号必须留下 skipped 记录（不再静默跳过）。
func TestRunCheckinNowLogsSkip(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1"}) // 故意不带 refreshToken
	s := New(Config{Pool: pl, LogPath: filepath.Join(t.TempDir(), "checkin.jsonl")})
	res := s.RunCheckinNowManual()
	if len(res) != 1 || res[0].Status != "skipped" {
		t.Fatalf("results=%+v want one skipped", res)
	}
	entries := s.RecentCheckins(10)
	if len(entries) != 1 || entries[0].Status != "skipped" || entries[0].UID != "u1" {
		t.Fatalf("log=%+v want one skipped entry for u1", entries)
	}
	if !entries[0].Manual || entries[0].Error == "" {
		t.Fatalf("entry=%+v want manual + reason", entries[0])
	}
}
