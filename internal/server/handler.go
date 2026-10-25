// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"trae2api-web/internal/pool"
	"trae2api-web/internal/scheduler"
	"trae2api-web/internal/upstream"
)

// 流式守护参数：
//   - heartbeatInterval：上游静默期间向客户端发 SSE 注释心跳的间隔（防中间代理/
//     客户端读超时）。注释行不进入 JSON 事件流，OpenAI 兼容 SDK 一律忽略。
//   - rotateBudget：流式请求「等上游首字节」阶段的总预算——预算内允许逐号轮换，
//     超出直接给流内 error 终态（历史上慢首字节集中爆发时曾出现逐号 ×120s 的
//     无限串行悬挂，客户端全程无字节）。
const (
	heartbeatInterval = 5 * time.Second
	rotateBudget      = 5 * time.Minute
)

// Config handler 依赖。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	APIKey       string        // 空 = 不鉴权
	AuthDir      string        // auths/ 目录，用于 import/delete 落盘 trae-*.json
	DataDir      string        // data/ 目录：使用日志 + 模型倍率落盘；空 = "data"
	MaxRotate    int           // 单请求最多换号次数，默认 3
	PlanCooldown time.Duration // 1005 冷却，默认 12h
	SoftCooldown time.Duration // 429 冷却，默认 60s
	ErrThreshold int           // 连续错误阈值，默认 3
	ErrCooldown  time.Duration // 错误冷却，默认 10m
	RefreshSkew  time.Duration // token 预刷新窗口，默认 24h
	DefaultModel string        // 默认 glm-5.2
	// Sched 定时调度器：/admin 手动签到/刷新接口的执行体；nil 时对应接口返回 501。
	Sched *scheduler.Scheduler
	// TransportCooldown 出口/网络故障（代理不可达等）的临时隔离时长，默认 5m。
	// 这类错误不累计 errCount，只把该出口从选号中短暂摘掉。
	TransportCooldown time.Duration
	// StreamIdleTimeout 上游 SSE 流「读空闲看门狗」：连续这么久从上游读不到任何
	// 新字节 → 主动断掉该条上游连接并按「流内中断」记 error 终态（2026-10-22：
	// 生产拍到 code=2001 流内挂 44 分钟、空心流最长 1256s——头已到但内容层一层
	// 不吐，ResponseHeaderTimeout 管不到中段）。大 context 预填正常只需 ≤30s
	// 出首事件，默认 120s 只切挂死不误杀慢响应；<=0 = 默认 120s。
	StreamIdleTimeout time.Duration
	// CallbackBase 登录回调 base URL（远程部署/反代场景，如 http://1.2.3.4:18080）；
	// 空 = 沿用旧行为 http://127.0.0.1:<port>/authorize。
	CallbackBase string
	// AdminPassword 面板登录密码（env TW2A_ADMIN_PASSWORD）；非空时 /admin/api/* 需登录
	// 或带 Bearer API Key。内置登录页，可替代 Nginx Basic Auth。
	AdminPassword string
	// AdminSessionDays 面板会话有效期（天，env TW2A_ADMIN_SESSION_DAYS）；
	// <=0 = 默认 7 天。
	AdminSessionDays int
	// LogRetentionDays 日志保留天数；0 = 关闭自动清理。
	LogRetentionDays int
	// CallRetentionMB 调用明细目录容量上限（MB）；0 = 不限量。
	CallRetentionMB int
	// Prompt 系统提示词策略：Mode "demote"（默认，system 恒定固定提示词 + 调用方
	// system 降级为对话首条 user 消息）/"replace"（调用方 system 丢弃）/"auto"
	// （有 system 透传）/ "trae"（合并注入）/ "off"（透传）；
	// File 自定义提示词文件（相对 data/ 或绝对路径），非空时覆盖内置提示词。
	Prompt PromptConfig
	// SanitizeFingerprints 出站请求体黑名单指纹清洗开关（默认 true）。
	SanitizeFingerprints bool
}

// maxBodyBytes 请求体大小上限（8MB），超过返回 413。
const maxBodyBytes = 8 << 20

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux

	usage   *UsageStore  // 使用日志（每笔模型请求）
	details *DetailStore // 调用明细存档（请求/返回/提示词，保留 30 天）
	proxies *ProxyPool   // 代理池（账号按名称选用）

	// Web 登录 pending 态：pendingID → 登录进行中的临时上下文。
	// 回调 /authorize 捕获后标记成功；面板轮询 result 取结果。
	loginMu sync.Mutex
	logins  map[string]*pendingLogin

	// admin 面板登录失败节流（仅设了密码时使用）。
	admin adminAuth

	// logs 数据维护状态（最近一次日志清理）。
	logs logMaintenance

	// charts 日粒度聚合缓存：call-details 增量摘要（侧车 data/chartdaily/<day>.json）。
	chartMu    sync.Mutex
	chartCache map[string]*dailySum

	// 模型级挂死熔断（fast-fail，2026-10-22 事故复盘）：同一模型窗内 N 次「出口/
	// 网络故障」（上游 chat 管道黑溺、缓冲不吐首字节）→ 熔断保持期内该模型请求
	// 立即 503，不再让客户端干等 2~4 分钟的 transport 超时（上游问题，账号轮换
	// 无解；换模型即可恢复，提示词放错误消息）。成功一次即清零观察窗。
	hangMu    sync.Mutex
	hangHits  map[string][]time.Time // 按 config_name 记录出口故障时刻序列
	hangUntil map[string]time.Time   // 熔断持有期截止（零值=不熔断）
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.PlanCooldown <= 0 {
		cfg.PlanCooldown = 12 * time.Hour
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 24 * time.Hour
	}
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = upstream.DefaultConfigName
	}
	if cfg.TransportCooldown <= 0 {
		cfg.TransportCooldown = 5 * time.Minute
	}
	if cfg.StreamIdleTimeout <= 0 {
		cfg.StreamIdleTimeout = 2 * time.Minute // 流内空闲看门狗默认 120s（不误杀大 context 预填 ≤30s 的正常首事件）
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "data"
	}
	if strings.TrimSpace(cfg.Prompt.Mode) == "" {
		cfg.Prompt.Mode = "auto"
	}
	h := &Handler{
		cfg:       cfg,
		mux:       http.NewServeMux(),
		logins:    map[string]*pendingLogin{},
		admin:     adminAuth{fails: map[string][]time.Time{}},
		hangHits:  map[string][]time.Time{},
		hangUntil: map[string]time.Time{},
		usage:     NewUsageStore(filepath.Join(cfg.DataDir, "usage.jsonl")),
		details:   NewDetailStore(filepath.Join(cfg.DataDir, "call-details"), int64(cfg.CallRetentionMB)*1024*1024),
		proxies:   NewProxyPool(filepath.Join(cfg.DataDir, "proxies.json")),
	}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	// 管理面板：本地面板
	// 读接口无鉴权（局域网内只读）；写接口（accounts 写/login/refresh/authorize）
	// 经 withAdminAuth 校验 Bearer = TW2A_API_KEY（见 §4 安全设计）。
	h.mux.HandleFunc("GET /admin", h.adminPage)
	// 面板登录（可选：TW2A_ADMIN_PASSWORD）
	h.mux.HandleFunc("GET /admin/session", h.adminSessionGet)
	h.mux.HandleFunc("POST /admin/login", h.adminLogin)
	h.mux.HandleFunc("POST /admin/logout", h.adminLogout)
	h.mux.HandleFunc("GET /admin/api/credits", h.adminCredits)
	// 账号 CRUD
	h.mux.HandleFunc("GET /admin/api/accounts", h.adminAccounts)
	h.mux.HandleFunc("POST /admin/api/accounts/import", h.withAdminAuth(h.adminImportAccount))
	h.mux.HandleFunc("DELETE /admin/api/accounts/{uid}", h.withAdminAuth(h.adminDeleteAccount))
	h.mux.HandleFunc("PATCH /admin/api/accounts/{uid}", h.withAdminAuth(h.adminPatchAccount))
	h.mux.HandleFunc("POST /admin/api/accounts/{uid}/refresh", h.withAdminAuth(h.adminRefreshAccount))
	h.mux.HandleFunc("POST /admin/api/accounts/{uid}/proxy_test", h.withAdminAuth(h.adminProxyTest))
	h.mux.HandleFunc("POST /admin/api/accounts/{uid}/device_register", h.withAdminAuth(h.adminRegisterDevice))
	h.mux.HandleFunc("GET /admin/api/accounts/{uid}/json", h.adminAccountJSON)
	// Web 登录闭环
	h.mux.HandleFunc("POST /admin/api/login", h.withAdminAuth(h.adminLoginStart))
	h.mux.HandleFunc("GET /admin/api/login/result", h.adminLoginResult)
	h.mux.HandleFunc("POST /admin/api/login/cancel", h.withAdminAuth(h.adminLoginCancel))
	// TRAE 回调落点（/authorize）：无需 Bearer（TRAE 浏览器 302 不带 key），
	// 仅捕获 query 写 pending 队列，不直接落盘 token。
	h.mux.HandleFunc("GET /authorize", h.authorizeCallback)
	// 手动触发定时任务（带鉴权；Sched 未装配返回 501）
	h.mux.HandleFunc("POST /admin/api/checkin_all", h.withAdminAuth(h.adminCheckinAll))
	h.mux.HandleFunc("POST /admin/api/refresh_all", h.withAdminAuth(h.adminRefreshAll))
	h.mux.HandleFunc("POST /admin/api/accounts/{uid}/checkin", h.withAdminAuth(h.adminCheckinOne))
	// 签到看板 / 数据维护
	h.mux.HandleFunc("GET /admin/api/checkin", h.adminCheckinBoard)
	h.mux.HandleFunc("GET /admin/api/logs", h.adminLogsGet)
	h.mux.HandleFunc("POST /admin/api/logs/prune", h.withAdminAuth(h.adminLogsPrune))
	// 使用日志 + 官方模型倍率 + 代理池
	h.mux.HandleFunc("GET /admin/api/usage", h.adminUsage)
	h.mux.HandleFunc("GET /admin/api/usage/detail", h.withAdminAuth(h.adminUsageDetail))
	h.mux.HandleFunc("GET /admin/api/charts", h.adminCharts)
	h.mux.HandleFunc("GET /admin/api/rates", h.adminRatesGet)
	// 模型菜单手动刷新（2026-10-22 口径：菜单自动刷新 = 每天一次随机时刻；
	// 面板按钮是唯一即时刷新路径）。
	h.mux.HandleFunc("POST /admin/api/models/refresh", h.withAdminAuth(h.adminModelsRefresh))
	h.mux.HandleFunc("GET /admin/api/proxies", h.withAdminAuth(h.adminProxiesGet))
	h.mux.HandleFunc("POST /admin/api/proxies", h.withAdminAuth(h.adminProxiesAdd))
	h.mux.HandleFunc("POST /admin/api/proxies/bulk", h.withAdminAuth(h.adminProxiesAddBulk))
	h.mux.HandleFunc("GET /admin/api/proxies/{name}/url", h.withAdminAuth(h.adminProxyURLGet))
	h.mux.HandleFunc("PUT /admin/api/proxies/{name}", h.withAdminAuth(h.adminProxiesUpdate))
	h.mux.HandleFunc("DELETE /admin/api/proxies/{name}", h.withAdminAuth(h.adminProxiesDelete))
	h.mux.HandleFunc("POST /admin/api/proxies/test", h.withAdminAuth(h.adminProxiesTest))
	h.mux.HandleFunc("POST /admin/api/proxies/test_all", h.withAdminAuth(h.adminProxiesTestAll))
	return h
}

// withAdminAuth 写操作鉴权：**有效会话 Cookie** 或 Bearer API Key 任一即可。
// 面板登录一次后写操作不再需要手动填 key（key 通道保留给脚本/curl）。
// 密码与 key 都没配时（本地裸用）不鉴权——保持历史行为。
func (h *Handler) withAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.authDisabled() || h.adminAuthorized(r) {
			next(w, r)
			return
		}
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "需要登录（或携带 Bearer API Key）")
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 配了面板密码时，/admin/api/* 一律要求登录（读接口也一样）；
	// 未配密码则保持历史行为（只读开放、写接口要 key）。
	if !h.adminGate(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "unauthorized", "需要登录面板（/admin）")
		return
	}
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if len(authz) < len(prefix) || !strings.EqualFold(authz[:len(prefix)], prefix) {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
			key := authz[len(prefix):]
			// 常量时间比较，防时序攻击（本地代理但按规范）。
			if subtle.ConstantTimeCompare([]byte(key), []byte(h.cfg.APIKey)) != 1 {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": h.cfg.Pool.List(),
	})
}

// ---------------------------------------------------------------------------
// 模型映射
// ---------------------------------------------------------------------------

// mapModel 将客户端传入的 model 映射为 config_name（SPEC §4.5）：
//
//	"glm-5.2"（config_name）        → 直接转发
//	"glm-5.2__dev"（内部名）        → 去掉后缀映射回 config_name
//	"auto" / ""                     → 默认模型
//	其他未知                        → 400
func (h *Handler) mapModel(model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" || model == "auto" {
		return h.cfg.DefaultModel, nil
	}
	// 去掉内部名后缀（__dev / __max 等）
	base := model
	if i := strings.Index(model, "__"); i >= 0 {
		base = model[:i]
	}
	if h.knownModel(base) {
		return base, nil
	}
	// 宽松匹配：下划线 → 横线，大小写不敏感（deepseek_v4_pro → DeepSeek-V4-Pro）
	norm := normalizeModelName(base)
	if h.knownModel(norm) {
		return norm, nil
	}
	return "", fmt.Errorf("unknown model %q", model)
}

// normalizeModelName 将下划线命名的内部名归一化为 config_name 风格（横线分隔）。
func normalizeModelName(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
	}
	return strings.Join(parts, "-")
}

// namespace 内部名 → config_name 归一化映射（下划线 → 横线）。

// knownModel 判断 model 是否在动态/静态模型表中。
//
// 禁止任何网络 IO：菜单拉取已移入后台定时任务（RunModelsRefresher）与
// /v1/models 的异步过期触发。聊天热路径只读内存，1.5.9（2026-10-22 事故复盘）：
// 此前每 1h TTL 过期后的首个聊天请求会同步拉一次菜单，出口抖动时全体卡
// ~10 秒、失败还临时回退静态表（缺 glm 家族别名）→ 秒回 400 unknown model。
func (h *Handler) knownModel(model string) bool {
	for _, m := range h.acceptListNoFetch() {
		if m["id"] == model {
			return true
		}
	}
	return false
}

// 静态 SOLO 模型表（SPEC P3：32 个 config_name，来自逆向报告；动态拉取失败时回退）。
//
// 2026-10-22 追加：OpenAI 风格别名 5 条（ll 上 ch18 渠道投放的稳定名单）。
// 上游 SOLO 接受这些：历史 roster 长期成员；冷启动/菜单空时兜底表若缺它们，
// glm/mimo/deepseek 系请求会被 mapModel 秒拒（unknow model）——1.5.8 事故
// （出口抖动窗口）就是静态兜底表缺名造成的。别名最终是否可用仍由上游裁决，
// 这里只决定「要不要把请求送出去」。
var staticModels = []map[string]any{
	{"id": "Doubao-Seed-2.1-Pro", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "seed-code-pro-0430", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "Doubao-Seed-2.1-Turbo", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "Doubao-Seed-2.0-Code", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "DeepSeek-V4-Flash-Official", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "browser_use_subagent", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "glm-5-turbo", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "glm-5", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "DeepSeek-V4-Pro", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "DeepSeek-V4-Flash", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "DeepSeek-V3.1", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "kimi-k3", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "kimi-k2.7-code", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "kimi-k2.6", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "kimi-k2.5", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "minimax-m2.7", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "minimax-m2.5", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "qwen-3.7-plus", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "qwen-3.6-plus", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "qwen-3.5", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "sagitta", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "aquila", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_gemini", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_placeholder", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_1M_text", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_1M", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_kimi", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_claude", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_gpt-5", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_no-fc", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_deepseek_chat", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_deepseek_reasoner", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "custom_model_deepseek_v4", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "explore_sub_agent_v13", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "explore_sub_agent_v2", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "summary", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	// ---- OpenAI 风格别名（2026-10-22 追加，ll 生产在投名单）----
	{"id": "glm-5.3-flash", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "glm-5.3-flashx", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "deepseek-v4.1-flash", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "mimo-v2.6-pro", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "mimo-v2.6-flash", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	// ---- 2026-10 客户端 1.107.1 逆向补齐（DLL 模型清单 vs 本表缺口，
	// docs/RESEARCH-CLIENT-REVERSE.md §5）：glm-5v-turbo、qwen3.8 系、
	// seed-2.1-pro-evolving。DLL 原样大小写收录，真实 config_name 形态未实测，
	// 最终可用性由动态菜单/上游裁决。----
	{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "qwen3.8-flash", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "qwen3.8-max", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "seed-2.1-pro-evolving", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "Doubao-Seed-Evolving", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
	{"id": "minimax-m3.1-flash", "object": "model", "created": 1753600000, "owned_by": "trae-solo", "context_length": 131072},
}

// dynamicModelsCache 动态模型菜单缓存（2026-10-22 口径改版）：
//
// 聊天热路径、/v1/models、/admin/api/rates 一律**只读本缓存**（零网络 IO）。菜单
// 的刷新来源只有两个：
//   - 每天一次随机时刻的后台拉取（RunModelsRefresher，24h 一次 + 启动随机偏移）；
//   - 面板「立即刷新」按钮（POST /admin/api/models/refresh）。
//
// 拉取失败**不清缓存**（继续用旧菜单兜服务，stale-can-serve）；仅冷启动（缓存为
// 空）时回落 staticModels 静态兜底表。1.5.8 的旧口径（每 1h TTL 过期后由聊天/
// 菜单热路径同步拉取、失败回退静态表）在出口抖动时会让全体请求卡 ~10s，且静态
// 表不含 glm 家族别名 → 秒回 400 unknown model，2026-10-22 生产确实如此。
var dynamicModelsCache struct {
	sync.RWMutex
	ids     []upstream.ModelInfo
	fetched time.Time
}

var modelsRefreshInFlight atomic.Bool // 同一时刻至多一次菜单拉取在飞（防双击/定时叠加）

// cachedModelInfos 读缓存里的动态菜单（只读，无 IO）。
func cachedModelInfos() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	out := dynamicModelsCache.ids
	dynamicModelsCache.RUnlock()
	return out
}

// acceptListNoFetch 当前可接受模型（OpenAI 格式）：缓存菜单优先；冷启动缓存为空
// → staticModels 静态兜底（含 glm 家族 OpenAI 别名，见该表头注释）。
func (h *Handler) acceptListNoFetch() []map[string]any {
	if infos := cachedModelInfos(); len(infos) > 0 {
		out := make([]map[string]any, 0, len(infos))
		for _, mi := range infos {
			entry := map[string]any{
				"id":             mi.ID,
				"object":         "model",
				"created":        1753600000,
				"owned_by":       "trae-solo",
				"context_length": mi.ContextWindow,
			}
			if entry["context_length"] == 0 {
				entry["context_length"] = 131072
			}
			out = append(out, entry)
		}
		return out
	}
	return staticModels
}

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.acceptListNoFetch(),
	})
}

// refreshModelMenu 同步拉一次菜单（get_detail_param）：成功 → 缓存更新（打 id
// 增删日志，运维可见上游 roster 变化）；失败 → 返回错误，缓存原样保留。
// 调用方只有两类：每天一次的后台定时（RunModelsRefresher 的 asyncRefreshMenu）
// 与面板手动刷新（adminModelsRefresh）。聊天路径永不到这里。
func (h *Handler) refreshModelMenu() (int, error) {
	acct := h.cfg.Pool.Pick()
	if acct == nil {
		return 0, fmt.Errorf("no healthy account to fetch the model menu")
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil {
		return 0, fmt.Errorf("fetch models: %w", err)
	}
	if len(infos) == 0 {
		return 0, fmt.Errorf("fetch models: empty roster from upstream")
	}
	dynamicModelsCache.Lock()
	old := dynamicModelsCache.ids
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	if len(infos) != len(old) {
		log.Printf("models menu refreshed: %d model(s) (was %d)", len(infos), len(old))
	} else {
		log.Printf("models menu refreshed: %d model(s)", len(infos))
	}
	return len(infos), nil
}

// asyncRefreshMenu 异步拉一次（并发上限 1，失败不炸不刷屏，仅一行日志）。
func (h *Handler) asyncRefreshMenu() {
	if !modelsRefreshInFlight.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer modelsRefreshInFlight.Store(false)
		if _, err := h.refreshModelMenu(); err != nil {
			log.Printf("models menu refresh failed (keep serving stale menu): %v", err)
		}
	}()
}

// RunModelsRefresher 菜单后台唯一自动刷新源：首次 = 启动后随机偏移 (0..24h)，
// 之后每 24h 一次——「每天最多一次、时刻随机」的口径（2026-10-22 定稿）。
// 面板「立即刷新」按钮是另一条人工路径；聊天热路径零 IO 由此架构保证。
func (h *Handler) RunModelsRefresher(ctx context.Context) {
	jitter := time.Duration(rand.Int63n(int64(24 * time.Hour)))
	log.Printf("models menu refresher: first pull in %v, then every 24h", jitter.Round(time.Minute))
	timer := time.NewTimer(jitter)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			h.asyncRefreshMenu()
			timer.Reset(24 * time.Hour)
		}
	}
}

// modelRate 返回模型的官方消耗倍率（get_detail_param 的 consumption_rate）；
// 上游未下发（0）或模型未知时返回 1。只读菜单缓存（零网络 IO；菜单由后台定时
// /面板手动刷新维护）。旧口径的「查不到就现拉一次」在聊天热路径上会卡请求，
// 已随 1.5.9 菜单架构改版移除。
func (h *Handler) modelRate(model string) float64 {
	for _, mi := range cachedModelInfos() {
		if mi.ID == model {
			if mi.Rate > 0 {
				return mi.Rate
			}
			return 1
		}
	}
	return 1
}

// ---------------------------------------------------------------------------
// chat
// ---------------------------------------------------------------------------

// setModelInBody 将 body 中 model 字段替换为 configName，并返回改写后的 body。
func setModelInBody(body []byte, configName string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	obj["model"] = configName
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// modelSupportsThinking 模型是否支持思考（batch 菜单 reasoning_effort_config）；
// 模型未知/菜单未下发 → true（保守透传，不误剥）。
func modelSupportsThinking(model string) bool {
	for _, mi := range cachedModelInfos() {
		if mi.ID == model {
			return mi.SupportThinking
		}
	}
	return true
}

// stripReasoningParams 对不支持思考的模型剥掉 reasoning_effort / reasoning_effort_level，
// 防上游 400 把调用方参数问题记成账号错误（2026-10-25 门控）。
func stripReasoningParams(body []byte) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	changed := false
	for _, k := range []string{"reasoning_effort", "reasoning_effort_level", "reasoning"} {
		if _, ok := obj[k]; ok {
			delete(obj, k)
			changed = true
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if len(body) > maxBodyBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 8MB limit")
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	configName, err := h.mapModel(peek.Model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// 模型级挂死熔断（fast-fail）：该模型最近反复「出口故障/上游黑溺」时，
	// 直接 503 让调用方秒切模型/渠道，不再陪上游黑洞干等 2~4 分钟 transport 超时。
	if h.hangBlocked(configName) {
		log.Printf("chat fast-fail: %s in hang hold", configName)
		writeOpenAIError(w, http.StatusServiceUnavailable, "model_temporarily_down",
			"model "+configName+" is currently unstable upstream (repeated stalls); retry later or switch model")
		return
	}
	if !modelSupportsThinking(configName) {
		body = stripReasoningParams(body)
	}
	body = setModelInBody(body, configName)
	// 系统提示词策略：统一注入固定的 Trae 风格提示词（单一 system，不叠加），
	// 让上游看到的请求前缀稳定 → 提示词缓存可跨请求命中 + 指纹一致。
	body = applyPromptPolicy(body, h.cfg.Prompt, h.cfg.DataDir)
	// 第二层：黑名单指纹清洗（demote 降级块/对话内容里的已知模板句），默认开启。
	if h.cfg.SanitizeFingerprints {
		body = SanitizeRequestBody(body)
	}

	// 使用日志：每笔请求一条（成功/失败都记），官方倍率 → 相对消耗
	start := time.Now()
	rate := h.modelRate(configName)
	var recUID string
	var recUsage map[string]any
	var recContent, recReasoning strings.Builder
	record := func(status, errMsg string) {
		p, comp, total, cRead, cCreate := parseTokenUsage(recUsage)
		h.usage.Add(UsageEntry{
			TS:               time.Now().UnixMilli(),
			UID:              recUID,
			Model:            configName,
			Stream:           peek.Stream,
			Status:           status,
			Error:            errMsg,
			PromptTokens:     p,
			CompletionTokens: comp,
			TotalTokens:      total,
			CacheRead:        cRead,
			CacheCreation:    cCreate,
			Rate:             rate,
			Cost:             round2(float64(total) * rate),
			DurationMs:       time.Since(start).Milliseconds(),
		})
		// 调用明细存档（30 天）：上游请求参数（保结构截断）+ 返回内容 + 系统提示词
		respUsage := recUsage
		if respUsage != nil {
			respUsage = upstream.NormalizeUsage(respUsage)
		}
		// 归档字段截断（2026-10-18 收紧）：明细均行曾经 ~200KB、单日 510MB；
		// 明细演化成「图表数据源」后有全扫暴行风险，正文/推理/系统词改保守上限。
		h.details.Add(CallDetail{
			TS:                time.Now().UnixMilli(),
			UID:               recUID,
			Model:             configName,
			UpstreamURL:       h.cfg.Upstream.AgentHost + upstream.EpChat,
			RequestBody:       archiveBody(upstream.PrepareBody(body)),
			SystemPrompt:      truncateStr(extractSystemPrompt(body), 8<<10),
			ResponseContent:   truncateStr(recContent.String(), 16<<10),
			ResponseReasoning: truncateStr(recReasoning.String(), 6<<10),
			Usage:             respUsage,
			Status:            status,
			Error:             errMsg,
			DurationMs:        time.Since(start).Milliseconds(),
		})
	}

	tried := map[string]bool{}
	var lastErr error
	var hangNoted bool // 本请求是否已给模型挂死观察窗记过一笔（双账号同轮失败只计一次）

	// 流式请求：先把 SSE 头 + 首个心跳注释发给客户端，再进轮换。
	// 之后上游首字节慢（120s 头等待悬挂）时客户端已拿 200 且线上有心跳流动，
	// 不再空等/断连；轮换与重试对客户端不可见，终态失败走流内 error 事件。
	// sw != nil 即「头已提交」，此后不得再发非 SSE 响应（401/413 等早校验在此前已完成）。
	var sw *heartbeatWriter
	var heartbeatDone chan struct{}
	if peek.Stream {
		if fl, ok := w.(http.Flusher); ok && fl != nil {
			h := w.Header()
			h.Set("Content-Type", "text/event-stream")
			h.Set("Cache-Control", "no-cache")
			h.Set("Connection", "keep-alive")
			h.Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			sw = &heartbeatWriter{w: w, fl: fl}
			_, _ = sw.Write([]byte(": ping\n\n"))
			sw.Flush()
			heartbeatDone = make(chan struct{})
			var hbWG sync.WaitGroup
			hbWG.Add(1)
			go func() {
				defer hbWG.Done()
				runHeartbeat(sw, heartbeatDone)
			}()
			defer func() {
				close(heartbeatDone)
				hbWG.Wait()
			}()
		}
	}
	var rotateDeadline time.Time
	if sw != nil {
		rotateDeadline = time.Now().Add(rotateBudget)
	}

	for i := 0; i < h.cfg.MaxRotate; i++ {
		if !rotateDeadline.IsZero() && time.Now().After(rotateDeadline) {
			lastErr = fmt.Errorf("upstream slow episode: rotation budget (%v) exhausted before any upstream data", rotateBudget)
			log.Printf("chat rotation: budget exhausted (%.0fs), stop rotating", rotateBudget.Seconds())
			break
		}
		acct := h.cfg.Pool.PickExcluding(tried)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		recUID = acct.UID

		// token 临近过期 → 先 refresh（持锁重查，避免并发重复轮换；失败冷却换号）
		refreshed, err := h.cfg.Upstream.RefreshTokenIfNeeded(acct, h.cfg.RefreshSkew)
		if err != nil {
			lastErr = err
			var ue *upstream.Error
			switch {
			case errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead:
				h.cfg.Pool.Disable(acct.UID, "refresh session dead")
			case upstream.IsTransportError(err):
				// 刷新走同一个出口，传输层失败同样不算账号错误。
				log.Printf("refresh uid=%s: 出口/网络故障（不计账号错误）: %v", acct.UID, shortErr(err))
				h.cfg.Pool.NoteTransportError(acct.UID, h.cfg.TransportCooldown, "刷新 token 出口故障: "+shortErr(err))
				// 落一行使用记录（不计成账/不扣零成本）：errs 透视才有「出口/网络故障」可看。
				record("error", "出口/网络故障（刷新token）: "+shortErr(err))
			default:
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolErr, h.cfg.ErrCooldown, "refresh: "+err.Error())
			}
			continue
		}
		if refreshed {
			_ = acct.SaveAtomic()
		}

		rc, status, respBody, terr := h.cfg.Upstream.ChatStream(r.Context(), acct, body)
		if terr != nil {
			lastErr = terr
			if upstream.IsTransportError(terr) {
				// 出口/网络故障（代理不可达、连接被重置、畸形响应…）不是账号的问题：
				// 不累计 errCount（否则出口抖一下就把好账号冷却甚至禁用），只把该出口临时隔离，
				// 本次请求靠 tried 换号继续。
				log.Printf("chat_stream uid=%s: 出口/网络故障（不计账号错误）: %v", acct.UID, shortErr(terr))
				h.cfg.Pool.NoteTransportError(acct.UID, h.cfg.TransportCooldown, "出口/网络故障: "+shortErr(terr))
				// 落一行使用记录：面板 errs 透视才看得到超时/出口风暴（2026-10-18 周日晚高峰实测缺口）。
				record("error", "出口/网络故障: "+shortErr(terr))
				// 模型级挂死观察窗：每请求记一笔（传输故障按请求去重，避免双账号
				// 同轮双计；账号轮换对「上游管道黑溺」无解，跨请求累计到阈值即熔断）。
				if !hangNoted {
					hangNoted = true
					h.noteModelHang(configName)
				}
			} else {
				h.cfg.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			}
			continue
		}
		if status >= 400 {
			kind := upstream.Classify(status, string(respBody))
			switch kind {
			case upstream.ErrPlanLimit:
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolPlan, h.cfg.PlanCooldown, "plan 权益不足")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			case upstream.ErrSoftRate:
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			case upstream.ErrSessionDead:
				h.cfg.Pool.Disable(acct.UID, "session dead")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			case upstream.ErrNotFound:
				// 404 短冷却不累计 errCount（防雪崩）
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			default:
				h.cfg.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			}
		}
		// 上游流内「读空闲看门狗」：大 context / 后端劣化会进入头已到、SSE 无事件的
		// 冻结态（生产实测 code=2001 挂 44 分钟、空心流 1256s）。120s 无字节即断流
		// 记 error，不给它挂到天荒地老（正常长响应逐字节重置计时，不受影响）。
		var idleW *idleWatchReader
		if h.cfg.StreamIdleTimeout > 0 {
			idleW = newIdleWatchReader(rc, h.cfg.StreamIdleTimeout)
			rc = idleW
		}
		if peek.Stream {
			h.cfg.Pool.NoteSuccess(acct.UID)
			// 流内业务错误（1005 plan/5xx 等）→ 冷却账号，错误信息注入 SSE。
			// sw（互斥包装）同时被心跳 goroutine 使用，避免与 chunk 写交错。
			var streamErrMsg string
			streamErr := upstream.StreamWithHooks(sw, rc, configName, func(se *upstream.SOLOStreamError) {
				h.handleStreamError(acct.UID, se)
				streamErrMsg = fmt.Sprintf("code=%d %s", se.Code, se.Msg)
			}, func(m map[string]any) { recUsage = m }, func(delta map[string]any) {
				if c, ok := delta["content"].(string); ok {
					recContent.WriteString(c)
				}
				if r, ok := delta["reasoning_content"].(string); ok {
					recReasoning.WriteString(r)
				}
			})
			rc.Close()
			// 流读取中断要落账并给终态（此前 err 被丢弃：上游冻结/断连会静默记成
			// "ok" + 0 token——「空心流」假阳性，正是 2026-10-22 复盘的实证之一）。
			switch {
			case idleW != nil && idleW.Killed() && streamErr != nil:
				streamErrMsg = fmt.Sprintf("上游流内冻结（%v 无新事件，看门狗断流）: %v", h.cfg.StreamIdleTimeout, shortErr(streamErr))
			case streamErr != nil:
				streamErrMsg = "上游流中断: " + shortErr(streamErr)
			}
			if streamErrMsg != "" {
				record("error", streamErrMsg)
				if sw != nil {
					// 流头已提交：终态以流内 error 事件 + [DONE] 收尾（客户端可见失败）。
					writeSSETerminalError(sw, streamErrMsg)
				}
			} else {
				record("ok", "")
			}
			h.noteModelOK(configName)
			return
		}
		resp, err := upstream.Aggregate(rc, configName)
		rc.Close() // 已完全消费，立即释放上游连接（防轮转 continue 泄漏 body）
		if err != nil {
			// 看门狗断流（非流式大响应中途冻结）→ 视作出口故障：换号重试不受
			// 「业务流错误不轮换」限制，且不累计账号错误。
			if idleW != nil && idleW.Killed() {
				lastErr = fmt.Errorf("上游流内冻结（看门狗断流，%v 无新数据）", h.cfg.StreamIdleTimeout)
				h.cfg.Pool.NoteTransportError(acct.UID, h.cfg.TransportCooldown, "上游流内冻结: "+shortErr(lastErr))
				continue
			}
			// 流内业务错误（如 1005 plan 权益不足）→ 冷却账号并轮转下一账号。
			var se *upstream.SOLOStreamError
			if errors.As(err, &se) {
				lastErr = err
				switch se.Kind() {
				case upstream.ErrPlanLimit:
					h.cfg.Pool.Cooldown(acct.UID, pool.CoolPlan, h.cfg.PlanCooldown, "plan 权益不足")
				default:
					h.cfg.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
				}
				continue
			}
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			return
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		if u, ok := resp["usage"].(map[string]any); ok {
			recUsage = u
		}
		// 非流式：抓返回正文供调用明细存档
		if choices, ok := resp["choices"].([]any); ok && len(choices) > 0 {
			if ch, ok := choices[0].(map[string]any); ok {
				if msg, ok := ch["message"].(map[string]any); ok {
					if c, ok := msg["content"].(string); ok {
						recContent.WriteString(c)
					}
					if r, ok := msg["reasoning_content"].(string); ok {
						recReasoning.WriteString(r)
					}
				}
			}
		}
		record("ok", "")
		h.noteModelOK(configName)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	record("error", msg)
	if sw != nil {
		// SSE 头已提交：终态以流内 error 事件 + [DONE] 收尾（客户端全程保持在线）。
		writeSSETerminalError(sw, msg)
		return
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

const (
	hangWindow    = 10 * time.Minute // 出口故障计窗
	hangThreshold = 2                // 窗内两笔**独立请求**都挂死 → 熔断（单次抖动不熔断，下一笔成功即清零）
	hangHold      = 5 * time.Minute  // 熔断保持时长（到期自动放行试恢复）
)

// noteModelHang 该模型发生一笔出口/网络故障（每请求至多记一笔）。窗内达到阈值 →
// 熔断保持期开启（后续该模型请求入口快速失败，不再陪上游黑洞干等 transport 超时）。
func (h *Handler) noteModelHang(model string) {
	h.hangMu.Lock()
	defer h.hangMu.Unlock()
	now := time.Now()
	hits := h.hangHits[model][:0]
	for _, t := range h.hangHits[model] {
		if now.Sub(t) < hangWindow {
			hits = append(hits, t)
		}
	}
	hits = append(hits, now)
	h.hangHits[model] = hits
	if len(hits) >= hangThreshold {
		h.hangUntil[model] = now.Add(hangHold)
		if len(hits) == hangThreshold { // 只在跨阈值瞬打一行，保持期内不刷屏
			log.Printf("chat hang guard: %s fast-fail engaged for %v (%d upstream stalls within %v)",
				model, hangHold, len(hits), hangWindow)
		}
	}
}

// noteModelOK 模型成功一笔 → 清观察窗与熔断（恢复信号）。
func (h *Handler) noteModelOK(model string) {
	h.hangMu.Lock()
	defer h.hangMu.Unlock()
	delete(h.hangHits, model)
	delete(h.hangUntil, model)
}

// hangBlocked 该模型是否处于挂死熔断保持期。
func (h *Handler) hangBlocked(model string) bool {
	h.hangMu.Lock()
	defer h.hangMu.Unlock()
	until, ok := h.hangUntil[model]
	return ok && !until.IsZero() && time.Now().Before(until)
}

// adminModelsRefresh POST /admin/api/models/refresh：面板「立即刷新」按钮。
// 同步拉一次菜单；并发已在飞 → 202 busy（前台秒回，不打断正在进行的拉取）。
func (h *Handler) adminModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if !modelsRefreshInFlight.CompareAndSwap(false, true) {
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "busy", "message": "menu refresh already in progress"})
		return
	}
	defer modelsRefreshInFlight.Store(false)
	n, err := h.refreshModelMenu()
	if err != nil {
		log.Printf("admin models refresh failed (keep serving stale menu): %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"status":  "error",
			"message": "菜单拉取失败（原菜单继续生效）: " + err.Error(),
			"count":   len(cachedModelInfos()),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"count":       n,
		"fetched_at":  time.Now().Format(time.RFC3339),
		"menu_source": menuSourceLabel(),
	})
}

// menuSourceLabel 当前菜单来源：dynamic（后台/手动拉取过）或 static（冷启动兜底）。
func menuSourceLabel() string {
	if len(cachedModelInfos()) > 0 {
		return "dynamic"
	}
	return "static"
}

// handleStreamError 流式响应中的上游业务错误 → pool 冷却状态机。
// 1005 plan 权益不足 → 长冷却；其余（5xx/参数错误等）→ 累计错误冷却。
func (h *Handler) handleStreamError(uid string, se *upstream.SOLOStreamError) {
	switch se.Kind() {
	case upstream.ErrPlanLimit:
		h.cfg.Pool.Cooldown(uid, pool.CoolPlan, h.cfg.PlanCooldown, "plan 权益不足")
	default:
		h.cfg.Pool.NoteError(uid, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// openAIErrorType 按状态码映射 OpenAI 错误 type（规范枚举）：
// 400/401/404 类请求问题 → invalid_request_error；429 → rate_limit_error；
// 5xx/其余 → api_error。恒为 api_error 会让按 error.type 分流的 SDK 误判类别。
func openAIErrorType(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound,
		http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    openAIErrorType(status),
			"code":    code,
		},
	})
}
