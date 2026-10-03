// config.go 加载 JSON 配置 + TW2A_* 环境变量覆盖。
// APIKey 只从环境变量 TW2A_API_KEY 读取（SPEC §0 脱敏纪律：key 走 env，不落盘 git）。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 顶层配置。
type Config struct {
	Listen        string `json:"listen"`        // ":7864"
	CallbackPort  string `json:"callback_port"` // "18080"（TRAE 登录回调监听端口，0 = 不起）
	CallbackBase  string `json:"callback_base"` // 登录回调 base URL（远程部署用，如 http://1.2.3.4:18080）；空 = 127.0.0.1:<port>
	APIKey        string `json:"-"`             // 只读 env TW2A_API_KEY（不读 json）
	AdminPassword string `json:"-"`             // 只读 env TW2A_ADMIN_PASSWORD（面板登录密码；空 = 不启用登录页）
	AuthDir       string `json:"auth_dir"`      // "./auths"
	StateFile     string `json:"state_file"`    // "./data/state.json"
	DefaultModel  string `json:"default_model"` // "glm-5.2"

	Prompt struct {
		Mode string `json:"prompt_mode"` // "trae" = 注入固定提示词（默认）；"off" = 透传
		File string `json:"prompt_file"` // 自定义提示词文件（相对 data/ 或绝对路径）
	} `json:"prompt"`

	Cooldown struct {
		PlanCredit  string `json:"plan_credit"`   // "12h"
		SoftRate    string `json:"soft_rate"`     // "60s"
		ErrThresh   int    `json:"err_threshold"` // 3
		ErrCooldown string `json:"err_cooldown"`  // "10m"
	} `json:"cooldown"`

	Schedule struct {
		CheckinHour       int   `json:"checkin_hour"`            // 旧字段（单时点），被 checkin_hours 取代
		CheckinHours      []int `json:"checkin_hours"`           // 签到时点列表，默认 [9]；多时点=失败重试窗口
		RefreshHours      []int `json:"refresh_hours"`           // [3]
		JitterMinutes     int   `json:"checkin_jitter_minutes"`  // 签到随机延迟窗口（分钟），默认 60，负数关闭
		BalanceRefreshMin int   `json:"balance_refresh_minutes"` // 余额后台刷新间隔，默认 30，0 关闭
	} `json:"schedule"`

	Upstream struct {
		TimeoutSeconds int `json:"timeout_seconds"` // 120
	} `json:"upstream"`

	// LogRetentionDays 日志保留天数（usage.jsonl / checkin.jsonl）：默认 90；0 = 不自动清理。
	LogRetentionDays int `json:"log_retention_days"`
	// CheckinWebhook 签到结果推送地址（企业微信/钉钉/飞书机器人）；空 = 不推送。
	CheckinWebhook string `json:"checkin_webhook"`
	// CheckinNotify 推送策略：fail（默认）/ always / never。
	CheckinNotify string `json:"checkin_notify"`

	// 解析后的 duration。
	PlanCreditDur  time.Duration `json:"-"`
	SoftRateDur    time.Duration `json:"-"`
	ErrCooldownDur time.Duration `json:"-"`
}

// Default 返回默认配置。
func Default() *Config {
	c := &Config{
		Listen:       ":7864",
		CallbackPort: "18080",
		APIKey:       "",
		AuthDir:      "./auths",
		StateFile:    "./data/state.json",
		DefaultModel: "glm-5.2",
	}
	c.Cooldown.PlanCredit = "12h"
	c.Cooldown.SoftRate = "60s"
	c.Cooldown.ErrThresh = 3
	c.Cooldown.ErrCooldown = "10m"
	c.Schedule.CheckinHour = 9
	c.Schedule.CheckinHours = []int{9}
	c.Schedule.RefreshHours = []int{3}
	c.Schedule.BalanceRefreshMin = 30
	c.Prompt.Mode = "auto"
	c.Upstream.TimeoutSeconds = 120
	c.LogRetentionDays = 90
	c.CheckinNotify = "fail"
	return c
}

// Load 从 path 读配置，再用 TW2A_* env 覆盖。path 为空或不存在时用默认 + env。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				// 配置文件可选：不存在 → 纯默认 + env
				c = Default()
			} else {
				return nil, fmt.Errorf("read config: %w", err)
			}
		} else if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("TW2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("TW2A_ADMIN_PASSWORD"); v != "" {
		c.AdminPassword = v
	}
	if v := os.Getenv("TW2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("TW2A_CALLBACK_PORT"); v != "" {
		c.CallbackPort = v
	}
	if v := os.Getenv("TW2A_CALLBACK_BASE"); v != "" {
		c.CallbackBase = v
	}
	if v := os.Getenv("TW2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("TW2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("TW2A_DEFAULT_MODEL"); v != "" {
		c.DefaultModel = v
	}
	if v := os.Getenv("TW2A_PLAN_CREDIT"); v != "" {
		c.Cooldown.PlanCredit = v
	}
	if v := os.Getenv("TW2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("TW2A_ERR_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Cooldown.ErrThresh = n
		}
	}
	if v := os.Getenv("TW2A_ERR_COOLDOWN"); v != "" {
		c.Cooldown.ErrCooldown = v
	}
	if v := os.Getenv("TW2A_CHECKIN_HOUR"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Schedule.CheckinHour = n
		}
	}
	if v := os.Getenv("TW2A_CHECKIN_HOURS"); v != "" {
		var hours []int
		for _, part := range strings.Split(v, ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil && n >= 0 && n <= 23 {
				hours = append(hours, n)
			}
		}
		if len(hours) > 0 {
			c.Schedule.CheckinHours = hours
		}
	}
	if v := os.Getenv("TW2A_BALANCE_REFRESH_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Schedule.BalanceRefreshMin = n
		}
	}
	if v := os.Getenv("TW2A_CHECKIN_JITTER_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Schedule.JitterMinutes = n
		}
	}
	if v := os.Getenv("TW2A_PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := os.Getenv("TW2A_PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
	if v := os.Getenv("TW2A_LOG_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.LogRetentionDays = n
		}
	}
	if v := os.Getenv("TW2A_CHECKIN_WEBHOOK"); v != "" {
		c.CheckinWebhook = v
	}
	if v := os.Getenv("TW2A_CHECKIN_NOTIFY"); v != "" {
		c.CheckinNotify = v
	}
	if v := os.Getenv("TW2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
}

func (c *Config) normalize() error {
	var err error
	if c.PlanCreditDur, err = time.ParseDuration(c.Cooldown.PlanCredit); err != nil {
		return fmt.Errorf("cooldown.plan_credit: %w", err)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.ErrCooldownDur, err = time.ParseDuration(c.Cooldown.ErrCooldown); err != nil {
		return fmt.Errorf("cooldown.err_cooldown: %w", err)
	}
	if c.Cooldown.ErrThresh <= 0 {
		c.Cooldown.ErrThresh = 3
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	if c.LogRetentionDays < 0 {
		c.LogRetentionDays = 0
	}
	if c.DefaultModel == "" {
		c.DefaultModel = "glm-5.2"
	}
	if c.Listen == "" {
		c.Listen = ":7864"
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// CallbackPort：空/未设 → 默认 18080；显式 "0" → 不起回调 server（纯手动粘贴模式）
	if c.CallbackPort == "" {
		c.CallbackPort = "18080"
	}
	return nil
}
