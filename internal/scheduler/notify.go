// notify.go 签到结果推送（可选，配 TW2A_CHECKIN_WEBHOOK 启用）。
//
// 按 URL 自动识别常见机器人格式（无需额外配置）：
//
//	qyapi.weixin.qq.com → 企业微信 {"msgtype":"text","text":{"content":...}}
//	oapi.dingtalk.com   → 钉钉（同企业微信格式，@手机号可自行加到 URL）
//	open.feishu.cn      → 飞书 {"msg_type":"text","content":{"text":...}}
//	其它                → 通用 {"title":...,"text":...}
package scheduler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Notifier 签到结果 webhook 推送器；nil / 空 URL = 关闭。
type Notifier struct {
	url  string
	kind string
	http *http.Client
}

// NewNotifier 按 URL 构建；空字符串返回 nil（调用方判空即可）。
func NewNotifier(rawURL string) *Notifier {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil
	}
	kind := "json"
	switch {
	case strings.Contains(rawURL, "qyapi.weixin.qq.com"):
		kind = "wecom"
	case strings.Contains(rawURL, "oapi.dingtalk.com"):
		kind = "dingtalk"
	case strings.Contains(rawURL, "feishu.cn"), strings.Contains(rawURL, "larksuite.com"):
		kind = "feishu"
	}
	return &Notifier{url: rawURL, kind: kind, http: &http.Client{Timeout: 10 * time.Second}}
}

// Kind 返回识别到的格式（面板/日志展示用）。
func (n *Notifier) Kind() string {
	if n == nil {
		return ""
	}
	return n.kind
}

// Payload 生成请求体（导出便于单测，不联网）。
func (n *Notifier) Payload(title, text string) []byte {
	if n == nil {
		return nil
	}
	content := text
	if title != "" {
		content = title + "\n" + text
	}
	var v any
	switch n.kind {
	case "feishu":
		v = map[string]any{"msg_type": "text", "content": map[string]any{"text": content}}
	case "wecom", "dingtalk":
		v = map[string]any{"msgtype": "text", "text": map[string]any{"content": content}}
	default:
		v = map[string]any{"title": title, "text": text}
	}
	raw, _ := json.Marshal(v)
	return raw
}

// Send 推送一条消息；未配置时直接返回 nil。
func (n *Notifier) Send(title, text string) error {
	if n == nil || n.url == "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodPost, n.url, bytes.NewReader(n.Payload(title, text)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}
