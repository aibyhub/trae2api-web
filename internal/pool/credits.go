// credits.go 积分余额轨迹（credits.jsonl）：
// 每当账号积分发生变化（签到补分 / 余额刷新 / 消耗后刷新）记一行
// {ts, uid, credits, delta, source}，面板图表用它画余额趋势。
// 追加写 + 按保留期裁剪（与 usage/checkin 共用 TW2A_LOG_RETENTION_DAYS）。
package pool

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// creditLine 积分变更单条记录（credits.jsonl 一行）。
type creditLine struct {
	TS      int64  `json:"ts"`               // unix 秒
	UID     string `json:"uid"`              // 账号
	Credits int64  `json:"credits"`          // 变更后的余额
	Delta   int64  `json:"delta"`            // 本次变化量（负 = 消耗）
	Source  string `json:"source,omitempty"` // checkin（签到后 EntUsage）| refresh（后台余额刷新）
}

// appendCreditLocked 追加一条轨迹（调用方持 p.mu；磁盘失败静默，不影响主流程）。
func (p *Pool) appendCreditLocked(l creditLine) {
	if p.creditFp == "" {
		return
	}
	raw, err := json.Marshal(l)
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.creditFp); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(p.creditFp, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

// CreditHistoryPath 轨迹文件路径（空 = 未记录）。
func (p *Pool) CreditHistoryPath() string { return p.creditFp }

// CreditHistoryStats 概况（条数 + 最早/最新 unix 秒）；未启用返回 entries=-1（面板跳过）。
func (p *Pool) CreditHistoryStats() (entries int, oldest, newest int64) {
	if p.creditFp == "" {
		return -1, 0, 0
	}
	raw, err := os.ReadFile(p.creditFp)
	if err != nil {
		return 0, 0, 0
	}
	for _, line := range bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n")) {
		var l creditLine
		if len(line) == 0 || json.Unmarshal(line, &l) != nil || l.TS == 0 {
			continue
		}
		entries++
		if oldest == 0 || l.TS < oldest {
			oldest = l.TS
		}
		if l.TS > newest {
			newest = l.TS
		}
	}
	return entries, oldest, newest
}

// PruneCreditHistory 按保留期裁剪轨迹（keepDays：< 0 不动作 / 0 清空 / > 0 保留最近 N 天）。
func (p *Pool) PruneCreditHistory(keepDays int) (removed, kept int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock() // 与 appendCreditLocked 互斥：防 ReadFile→Rename 窗口丢追加行（Windows 下 rename 直接失败）
	if keepDays < 0 || p.creditFp == "" {
		return 0, 0, nil
	}
	raw, err := os.ReadFile(p.creditFp)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	var cutoff time.Time
	if keepDays > 0 {
		cutoff = time.Now().AddDate(0, 0, -keepDays)
	}
	var keep bytes.Buffer
	for _, line := range bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var l creditLine
		if json.Unmarshal(line, &l) != nil || keepDays == 0 || (keepDays > 0 && time.Unix(l.TS, 0).Before(cutoff)) {
			removed++
			continue
		}
		keep.Write(line)
		keep.WriteByte('\n')
		kept++
	}
	if removed == 0 {
		return removed, kept, nil
	}
	if kept == 0 {
		if err := os.Remove(p.creditFp); err != nil {
			return removed, kept, err
		}
		return removed, kept, nil
	}
	tmp := p.creditFp + ".tmp"
	if err := os.WriteFile(tmp, keep.Bytes(), 0o600); err != nil {
		return removed, kept, err
	}
	return removed, kept, os.Rename(tmp, p.creditFp)
}
