// client.go SOLO 上游客户端：llm_utils_chat / get_detail_param / ExchangeToken /
// checkin_credits / ide_user_ent_usage + 错误分类。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"trae2api-web/internal/auth"
)

// ErrKind 错误分类，pool 据此决定冷却时长（SPEC §4.3）。
type ErrKind int

const (
	ErrNone        ErrKind = iota // 成功
	ErrPlanLimit                  // 1005 + plan → 权益不足（硬冷却 12h）
	ErrSoftRate                   // 429 → 短冷却 60s
	ErrSessionDead                // 401 + Cloud-IDE-JWT 失效 → 禁用
	ErrNotFound                   // 404 → 短冷却 60s 不累计 errCount
	ErrServer                     // 5xx
	ErrClient                     // 其他 4xx
)

func (k ErrKind) String() string {
	switch k {
	case ErrPlanLimit:
		return "plan_limit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

var sessionDeadMarkers = []string{"login", "token 失效", "token invalid", "session", "unauthorized", "401"}

// Classify 按 HTTP 状态码 + body 判定错误类别（SPEC §4.3）。
func Classify(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	// 1005 plan 权益不足
	if strings.Contains(body, `"code":1005`) || (strings.Contains(body, "1005") && strings.Contains(lower, "plan")) {
		return ErrPlanLimit
	}
	// session 失效
	if status == http.StatusUnauthorized {
		for _, m := range sessionDeadMarkers {
			if strings.Contains(lower, strings.ToLower(m)) {
				return ErrSessionDead
			}
		}
		return ErrSessionDead
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

// Client SOLO 上游 HTTP 客户端。Host 字段可覆盖便于测试。
type Client struct {
	// HTTP 用于短 JSON 请求（ExchangeToken/模型/签到/积分），有总超时兜底。
	HTTP *http.Client
	// StreamHTTP 用于 SSE 流式对话：不设总超时，避免长流被截断；
	// 通过 Transport.ResponseHeaderTimeout 兜底「上游一直不返回首字节」的悬挂。
	// 与 HTTP 共享同一 Transport（连接池复用）。nil 时 ChatStream 回退 HTTP。
	StreamHTTP *http.Client

	AgentHost string // https://trae-api-cn.mchost.guru
	UgHost    string // https://api.trae.cn
	OAuthHost string // https://api.trae.com.cn
	ClientID  string // en1oxy7wnw8j9n

	// proxyClients 按代理 URL 缓存的客户端对（std/stream），支撑每账号独立出口。
	proxyClients sync.Map // string → *proxyPair
}

// proxyPair 一个代理出口对应的客户端对；std 有总超时（短请求），stream 无（SSE）。
type proxyPair struct{ std, stream *http.Client }

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second, // 首字节兜底（长推理预留），不限制整流时长
	}
	return &Client{
		HTTP:       &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr}, // 无总超时
		AgentHost:  AgentHost,
		UgHost:     UgHost,
		OAuthHost:  OAuthHost,
		ClientID:   ClientID,
	}
}

func (c *Client) agentBase() string { return c.AgentHost }
func (c *Client) ugBase() string    { return c.UgHost }
func (c *Client) oauthBase() string { return c.OAuthHost }

// proxyEnv TW2A_PROXY_URL 全局兜底代理（账号未配置 proxyUrl 时使用）。
// 环境变量进程内不变，读一次缓存。
var (
	proxyEnvOnce sync.Once
	proxyEnvVal  string
)

func proxyEnv() string {
	proxyEnvOnce.Do(func() { proxyEnvVal = strings.TrimSpace(os.Getenv("TW2A_PROXY_URL")) })
	return proxyEnvVal
}

// clientFor 返回该账号可用的 HTTP 客户端（按账号独立代理出口）。
// 优先级：auth 文件 proxyUrl > TW2A_PROXY_URL > 直连（默认客户端）。
// 代理 URL 支持 http/https/socks5；非法配置记日志后直连（结果同样缓存，避免刷日志）。
// 默认路径（无代理）与 v1.1.5 之前行为完全一致，测试注入的 mock 客户端不受影响。
func (c *Client) clientFor(a *auth.Auth) (std, stream *http.Client) {
	std, stream = c.HTTP, c.StreamHTTP
	if stream == nil {
		stream = std
	}
	proxy := ""
	if a != nil {
		proxy = strings.TrimSpace(a.ProxyURL)
	}
	if proxy == "" {
		proxy = proxyEnv()
	}
	if proxy == "" {
		return std, stream
	}
	if v, ok := c.proxyClients.Load(proxy); ok {
		p := v.(*proxyPair)
		return p.std, p.stream
	}
	u, err := url.Parse(proxy)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5") {
		log.Printf("proxy config invalid (%q) — fallback to direct", proxy)
		p := &proxyPair{std: std, stream: stream}
		c.proxyClients.Store(proxy, p)
		return p.std, p.stream
	}
	var tr *http.Transport
	if base, ok := c.HTTP.Transport.(*http.Transport); ok && base != nil {
		tr = base.Clone() // 复制连接池/超时兜底设置（Clone 不复制连接），再覆盖代理
	} else {
		tr = &http.Transport{}
	}
	tr.Proxy = http.ProxyURL(u)
	p := &proxyPair{
		std:    &http.Client{Timeout: c.HTTP.Timeout, Transport: tr},
		stream: &http.Client{Transport: tr}, // 无总超时（SSE）
	}
	c.proxyClients.Store(proxy, p)
	return p.std, p.stream
}

// doJSON 发请求并解 JSON；HTTP 非 2xx 时返回带 body 片段的 *Error。
// 传输客户端按账号代理解析（clientFor）。
func (c *Client) doJSON(req *http.Request, a *auth.Auth) (json.RawMessage, error) {
	hc, _ := c.clientFor(a)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	return raw, nil
}

// RefreshToken 通过 ExchangeToken 强制刷新 access token（refreshToken 轮换）。
// 成功时更新 a 的字段；调用方负责 SaveAtomic。全程持 a 写锁。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	return c.refreshLocked(a)
}

// RefreshTokenIfNeeded 仅当 token 在 skew 内即将过期（或已过期）时才刷新，
// 返回是否真正刷新。持锁内重查，避免并发请求对同一账号重复 ExchangeToken 轮换。
// 调用方仅在 returned 为 true 时需要 SaveAtomic。
func (c *Client) RefreshTokenIfNeeded(a *auth.Auth, skew time.Duration) (bool, error) {
	a.Lock()
	defer a.Unlock()
	if !a.NeedsRefreshLocked(skew) {
		return false, nil
	}
	if err := c.refreshLocked(a); err != nil {
		return false, err
	}
	return true, nil
}

// refreshLocked 是 RefreshToken 的持锁内部实现；调用方必须已持有 a 写锁。
// 任何失败路径都不改写 a 字段，保证旧 refreshToken 可重试。
func (c *Client) refreshLocked(a *auth.Auth) error {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	}
	body := map[string]any{
		"ClientID":     c.ClientID,
		"RefreshToken": a.RefreshToken, // 已持 a 写锁，直接读
		"ClientSecret": "-",
		"UserID":       "",
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpExchange, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	OAuthHeaders(req)
	data, err := c.doJSON(req, a)
	if err != nil {
		return err
	}
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
			RefreshExpireAt     int64  `json:"RefreshExpireAt"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("exchange parse: %w", err)
	}
	if resp.Result.Token == "" {
		return fmt.Errorf("refresh_failed: no token in response — re-login required")
	}
	a.AccessToken = resp.Result.Token
	if resp.Result.RefreshToken != "" {
		a.RefreshToken = resp.Result.RefreshToken
	}
	// 过期时间：优先 TokenExpireAt（上游返回毫秒，需归一化为 Unix 秒）
	if resp.Result.TokenExpireAt > 0 {
		a.ExpiresAt = normalizeExpiresAt(resp.Result.TokenExpireAt)
	} else if resp.Result.TokenExpireDuration > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(resp.Result.TokenExpireDuration) * time.Second).Unix()
	}
	return nil
}

// normalizeExpiresAt 把 ExchangeToken 的 TokenExpireAt 归一化为 Unix 秒。
// 上游返回毫秒（如 1786847930141），auth 文件用秒（1786847930）。
// 毫秒时间戳 ~1.7e12，秒时间戳 ~1.7e9，用 1e12 区分。
func normalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// ChatStream 发 llm_utils_chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 Classify）、err 为 nil；
// 只有传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpChat, bytes.NewReader(PrepareBody(body)))
	if err != nil {
		return nil, 0, nil, err
	}
	SOLOHeaders(req, a, true)
	// 用专用流客户端（无总超时），避免长 SSE 流被 HTTP.Timeout 截断；走账号代理出口。
	_, sc := c.clientFor(a)
	hc := sc
	if hc == nil {
		hc = c.HTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream %d %s body=%s",
			a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// ModelInfo 动态模型信息。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64   // = maxInputTokens
	MaxTokens     int64   // = maxOutputTokens
	Rate          float64 // 官方消耗倍率（display_contact_config.consumption_rate.data.rate），0 = 上游未下发
	FeeLevel      int     // display_config.fee_model_level
}

// FetchModels 拉 SOLO 模型表（get_detail_param，32 配置）。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	body := map[string]any{
		"function":            Function,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpModels, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	SOLOHeaders(req, a, false)
	data, err := c.doJSON(req, a)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ConfigInfoList []struct {
			ConfigName    string `json:"config_name"`
			DisplayConfig struct {
				DisplayName   string `json:"display_name"`
				FeeModelLevel int    `json:"fee_model_level"`
			} `json:"display_config"`
			// display_contact_config 是内嵌 JSON 字符串，consumption_rate.data.rate
			// 为该模型的官方积分消耗倍率（实测 Doubao-Seed-Evolving=0.08 等）。
			DisplayContactConfig string `json:"display_contact_config"`
			ModelDetailList      []struct {
				ModelName string `json:"model_name"`
			} `json:"model_detail_list"`
		} `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	var out []ModelInfo
	for _, cfg := range resp.ConfigInfoList {
		if cfg.ConfigName == "" {
			continue
		}
		rate := 0.0
		if cfg.DisplayContactConfig != "" {
			var contact struct {
				ConsumptionRate struct {
					Enable bool `json:"enable"`
					Data   struct {
						Rate float64 `json:"rate"`
					} `json:"data"`
				} `json:"consumption_rate"`
			}
			if json.Unmarshal([]byte(cfg.DisplayContactConfig), &contact) == nil && contact.ConsumptionRate.Enable {
				rate = contact.ConsumptionRate.Data.Rate
			}
		}
		out = append(out, ModelInfo{
			ID:       cfg.ConfigName,
			Name:     cfg.DisplayConfig.DisplayName,
			Rate:     rate,
			FeeLevel: cfg.DisplayConfig.FeeModelLevel,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// ugCheckinBody status/claim 的请求体。
// req_source 逆向自客户端 main.js：`Pr(this.P)?2:1`，Pr=产品线判定
// （Gje() 明示 trae_client: Lite↔req_source=2 / IDE↔1）。网关模拟 solo 产品线
// （solo_work_lite），必须发 2；旧值 1 是 IDE 档，v1.1.3 及之前为臆测值。
var ugCheckinBody = []byte(`{"req_source":2}`)

// CheckinStatus 查询签到状态。
// ug 通道业务码随 HTTP 200 返回，需同时解析 code 与字段（兼容 data 嵌套形态）。
func (c *Client) CheckinStatus(a *auth.Auth) (checkedIn bool, credits int64, enable bool, err error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinStatus, bytes.NewReader(ugCheckinBody))
	if err != nil {
		return false, 0, false, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req, a)
	if err != nil {
		return false, 0, false, err
	}
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
		CheckedIn bool  `json:"checked_in"`
		Credits   int64 `json:"credits"`
		Enable    bool  `json:"enable"`
		Data      *struct {
			CheckedIn bool  `json:"checked_in"`
			Credits   int64 `json:"credits"`
			Enable    bool  `json:"enable"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, 0, false, fmt.Errorf("checkin status parse: %w", err)
	}
	if resp.Code != 0 {
		return false, 0, false, fmt.Errorf("checkin status: code=%d %s", resp.Code, ugMsg(resp.Message, resp.Msg))
	}
	if resp.Data != nil {
		return resp.Data.CheckedIn, resp.Data.Credits, resp.Data.Enable, nil
	}
	return resp.CheckedIn, resp.Credits, resp.Enable, nil
}

// CheckinClaim 执行签到。
// 两个实测坑（见 RESEARCH §3/§5 与公开复盘）：
//   - 业务码随 HTTP 200 返回（如 9074），只看 HTTP 状态码会把失败当成功——必须解析 body 的 code
//   - claim 必须带 X-Device-Id 头（缺失 → 9004「order parameters incorrect」；
//     设备号未被账号注册 → 9074「当前参与用户太多」为通用反滥用文案，非真实容量信号，
//     确定性拒绝，重试无意义——调用方不要对 9074 做重试）。
func (c *Client) CheckinClaim(a *auth.Auth) error {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinClaim, bytes.NewReader(ugCheckinBody))
	if err != nil {
		return err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req, a)
	if err != nil {
		return err
	}
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		// HTTP 200 但 body 非 JSON：无法判读，保持旧行为按成功处理
		return nil
	}
	if resp.Code != 0 {
		return fmt.Errorf("checkin claim: code=%d %s", resp.Code, ugMsg(resp.Message, resp.Msg))
	}
	return nil
}

// ugMsg 取 message/msg 中非空者（上游两种字段名都出现过）。
func ugMsg(message, msg string) string {
	if strings.TrimSpace(message) != "" {
		return message
	}
	return msg
}

// UserEntUsage 聚合积分（ide_user_ent_usage 的 credits_limit 求和）。
func (c *Client) UserEntUsage(a *auth.Auth) (remain int64, err error) {
	remain, _, _, _, err = c.EntUsage(a)
	return remain, err
}

// EntPack 单个权益包明细（面板展示：名称/额度/已用/过期时间）。
type EntPack struct {
	Name     string `json:"name"`      // display_desc，如「签到奖励」
	Group    string `json:"group"`     // group_name，如「每日签到」
	Limit    int64  `json:"limit"`
	Used     int64  `json:"used"`
	ExpireAt int64  `json:"expire_at"` // unix 秒；0 = 未知
	Status   int    `json:"status"`
}

// EntUsageDetail 查询权益包全量明细（含过期时间；实测字段 display_desc /
// group_name / expire_time（unix 秒）/ entitlement_base_info.quota.credits_limit /
// usage.credits_amount）。remain = Σ(limit-used)，仅统计 limit>0 的包。
func (c *Client) EntUsageDetail(a *auth.Auth) (packs []EntPack, remain, limit, used int64, err error) {
	// 请求体对齐真实客户端（main.js pb()）：{require_usage:true, req_source:2}
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpEntUsage,
		bytes.NewReader([]byte(`{"require_usage":true,"req_source":2}`)))
	if err != nil {
		return nil, 0, 0, 0, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req, a)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	var resp struct {
		UserEntitlementPackList []struct {
			DisplayDesc string `json:"display_desc"`
			GroupName   string `json:"group_name"`
			ExpireTime  int64  `json:"expire_time"`
			Status      int    `json:"status"`
			EntitlementBaseInfo struct {
				Quota struct {
					CreditsLimit int64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, 0, 0, 0, fmt.Errorf("ent usage parse: %w", err)
	}
	for _, p := range resp.UserEntitlementPackList {
		l := p.EntitlementBaseInfo.Quota.CreditsLimit
		if l <= 0 {
			continue
		}
		u := int64(p.Usage.CreditsAmount)
		limit += l
		used += u
		remain += l - u
		packs = append(packs, EntPack{
			Name: p.DisplayDesc, Group: p.GroupName,
			Limit: l, Used: u, ExpireAt: p.ExpireTime, Status: p.Status,
		})
	}
	return packs, remain, limit, used, nil
}

// EntUsage 查询账号额度明细（积分总量/已用/剩余/权益包数）。
func (c *Client) EntUsage(a *auth.Auth) (remain, limit, used int64, packs int, err error) {
	ps, remain, limit, used, err := c.EntUsageDetail(a)
	return remain, limit, used, len(ps), err
}

// GetUserInfo 查询账号信息（登录用）。
func (c *Client) GetUserInfo(a *auth.Auth) (uid, nickname, enterpriseID string, err error) {
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	}
	body := map[string]any{"ReqSource": "IDE", "IDEVersion": IdeVersion}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpUserInfo, bytes.NewReader(raw))
	if err != nil {
		return "", "", "", err
	}
	OAuthHeaders(req)
	req.Header.Set("X-Cloudide-Token", a.JWT()) // 读锁快照
	data, err := c.doJSON(req, a)
	if err != nil {
		return "", "", "", err
	}
	var resp struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", "", fmt.Errorf("userinfo parse: %w", err)
	}
	return resp.Result.UserID, resp.Result.ScreenName, resp.Result.EnterpriseID, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
