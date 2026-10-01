// accounts.go /admin/api/accounts 全套：列表 / 导入 / 删除 / PATCH 开关 / 刷新 / JSON 脱敏预览。
//
// 安全纪律（PLAN §4）：
//   - 列表与 JSON 预览绝不返回完整 token，只给前缀 + 长度
//   - 写操作经 withAdminAuth（Bearer 校验）
//   - 回调链接只在服务端解析，前端不经手敏感数据
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"trae2api-web/internal/auth"
	"trae2api-web/internal/upstream"
)

// accountSummary 列表/预览对外结构（脱敏）。
type accountSummary struct {
	UID          string `json:"uid"`
	Nickname     string `json:"nickname,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	Enabled      bool   `json:"enabled"`
	Disabled     bool   `json:"disabled"` // session dead 硬禁用
	Cooling      bool   `json:"cooling"`
	Reason       string `json:"reason,omitempty"`
	Credits      int64  `json:"credits"`
	ErrCount     int    `json:"err_count,omitempty"`
	// Token 有效期（Unix 秒）与是否临近过期
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	ExpiredSoon  bool   `json:"expired_soon,omitempty"`
	ExpireAt     int64  `json:"credit_expire_at,omitempty"` // 最近仍有余量的权益包过期时刻（unix 秒）；0 = 未知
	MachineID    string `json:"machine_id,omitempty"`       // 前 8 位（脱敏）
	DeviceID     string `json:"device_id,omitempty"`        // 前 8 位（脱敏）
	Proxy        string `json:"proxy,omitempty"`            // 出口代理（userinfo 脱敏），空 = 直连/全局兜底
	HasAuth      bool   `json:"has_auth"`
}

// adminAccounts GET /admin/api/accounts：列表（无鉴权，只读）。
func (h *Handler) adminAccounts(w http.ResponseWriter, r *http.Request) {
	statuses := h.cfg.Pool.List()
	out := make([]accountSummary, 0, len(statuses))
	for _, s := range statuses {
		sum := accountSummary{
			UID:       s.UID,
			Nickname:  s.Nickname,
			Enabled:   s.Enabled,
			Disabled:  s.Disabled,
			Cooling:   s.Cooling,
			Reason:    s.Reason,
			Credits:   s.Credits,
			ErrCount:  s.ErrCount,
		}
		sum.ExpireAt = s.ExpireAt // 积分包最近过期时刻（选号「先过期优先」的依据）
		if a := h.cfg.Pool.AuthByUID(s.UID); a != nil {
			sum.HasAuth = true
			sum.ExpiresAt = a.ExpiresAt
			if a.ExpiresAt > 0 && a.NeedsRefresh(24*60*60) { // 24h 内过期
				sum.ExpiredSoon = true
			}
			sum.MachineID = prefix(a.MachineID, 8)
			sum.DeviceID = prefix(a.DeviceID, 8)
			sum.Proxy = maskProxy(a.ProxyURL)
		}
		out = append(out, sum)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// importRequest 导入请求体。
type importRequest struct {
	// 三选一：
	//   CallbackURL：TRAE 登录回调链接（http://127.0.0.1:.../authorize?refreshToken=...）
	//   JSON：嵌套形（{"account":..,"auth":..}）或扁平形（{"accessToken":..,"uid":..}）
	//   CallbackURL 与 JSON 都给时优先 CallbackURL。
	CallbackURL string `json:"callback_url,omitempty"`
	JSON        string `json:"json,omitempty"`
	// 可选覆盖字段（导入扁平 JSON 手建场景补 machine/device）
	MachineID string `json:"machine_id,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
	ProxyURL  string `json:"proxy_url,omitempty"` // 本账号独立出口代理（http/https/socks5）
}

// importResult 导入结果。
type importResult struct {
	UID       string `json:"uid"`
	Nickname  string `json:"nickname,omitempty"`
	Action    string `json:"action"` // "created" | "updated"
	NeedsCheck bool  `json:"needs_check,omitempty"` // 建议用户确认额度
}

// adminImportAccount POST /admin/api/accounts/import：导入凭证。
// body 三选一：回调链接 / 嵌套 JSON / 扁平 JSON。
func (h *Handler) adminImportAccount(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req importRequest
	// 先尝试 JSON 解析（{...}）；失败则当回调链接字符串
	trim := strings.TrimSpace(string(body))
	if strings.HasPrefix(trim, "{") {
		if err := json.Unmarshal(body, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse json: "+err.Error())
			return
		}
	} else {
		// 纯字符串 body → 当回调链接
		req.CallbackURL = trim
	}

	if h.cfg.AuthDir == "" {
		writeOpenAIError(w, http.StatusInternalServerError, "no_auth_dir", "server AuthDir not configured")
		return
	}

	var a *auth.Auth
	switch {
	case req.CallbackURL != "":
		a, err = h.importFromCallback(req)
	case req.JSON != "":
		a, err = h.importFromJSON(req)
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "need callback_url or json")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "import_failed", err.Error())
		return
	}
	if a.FilePath == "" {
		a.FilePath = auth.FilePathFor(h.cfg.AuthDir, a.UID)
	}

	// 落盘（原子写，复用 auth.SaveAtomic）
	if err := os.MkdirAll(h.cfg.AuthDir, 0o755); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "mkdir_failed", err.Error())
		return
	}
	_, statErr := os.Stat(a.FilePath)
	if err := a.SaveAtomic(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	action := "created"
	if statErr == nil {
		action = "updated"
	}
	h.cfg.Pool.Add(a)

	writeJSON(w, http.StatusOK, importResult{
		UID: a.UID, Nickname: a.Nickname, Action: action, NeedsCheck: true,
	})
}

// importFromCallback 解析回调链接 → ExchangeToken → GetUserInfo → 构造 Auth。
func (h *Handler) importFromCallback(req importRequest) (*auth.Auth, error) {
	info, err := ParseCallback(req.CallbackURL)
	if err != nil {
		return nil, err
	}
	// 无 refreshToken 但有 accessToken（userJwt 兜底）→ 直接用，不 ExchangeToken
	a := &auth.Auth{
		AccessToken:  info.AccessToken,
		RefreshToken: info.RefreshToken,
		UID:         info.UID,
		Nickname:    info.Nickname,
		EnterpriseID: info.EnterpriseID,
		Domain:       "trae.cn",
		ApiHost:      "https://api.trae.com.cn",
		MachineID:    req.MachineID,
		DeviceID:     req.DeviceID,
		ExpiresAt:    info.ExpiresAt,
	}
	// 有 refreshToken → ExchangeToken 换新 access token（轮换 refreshToken）
	if a.RefreshToken != "" {
		if err := h.cfg.Upstream.RefreshToken(a); err != nil {
			return nil, errors.New("exchange_token: " + err.Error())
		}
	}
	// GetUserInfo 补全 uid/nickname/enterpriseId（回调 userInfo 可能缺）
	uid, nick, ent, err := h.cfg.Upstream.GetUserInfo(a)
	if err == nil && uid != "" {
		a.UID = uid
		if nick != "" {
			a.Nickname = nick
		}
		if ent != "" {
			a.EnterpriseID = ent
		}
	}
	if a.UID == "" {
		return nil, errors.New("cannot determine uid from callback or GetUserInfo")
	}
	if a.AccessToken == "" {
		return nil, errors.New("no access token after exchange")
	}
	// 补设备身份：签到 claim 必须带十进制 x-device-id（12~20 位数字）。
	// hex32/UUID 会被上游 9074 拒（「当前参与用户太多」为误导文案，
	// 见 upstream/deviceid.go）；缺失或非十进制一律重新生成，
	// machine_id 保持 hex32 不动。每账号独立设备号——上游按
	// 「每设备每日一次」跨账号记账。
	genDev := false
	if a.MachineID == "" {
		if mid, rerr := randomHex(16); rerr == nil {
			a.MachineID = mid
			genDev = true
		}
	}
	if !upstream.IsDecimalDeviceID(a.DeviceID) {
		if did, rerr := upstream.NewDeviceID(); rerr == nil {
			a.DeviceID = did
			genDev = true
		}
	}
	if genDev {
		log.Printf("import: uid=%s 已补齐设备身份（device 为本账号独立十进制号）", a.UID)
	}
	// 设备注册：上游对部分（新）账号要求「注册过的设备」，未注册号 claim 一律 9074。
	// 导入时自动向上游注册一台本账号专属设备并激活；失败退回生成的十进制号。
	if did, rerr := h.cfg.Upstream.RegisterDevice(a); rerr == nil && upstream.IsDecimalDeviceID(did) {
		a.DeviceID = did
		log.Printf("import: uid=%s 已注册独立设备号 %s", a.UID, did)
	} else if rerr != nil {
		log.Printf("import: uid=%s 设备注册失败（退回随机号）: %v", a.UID, rerr)
	}
	return a, nil
}

// importFromJSON 解析嵌套/扁平 JSON → 构造 Auth（凭证已完整，不 ExchangeToken）。
func (h *Handler) importFromJSON(req importRequest) (*auth.Auth, error) {
	a, err := auth.Parse([]byte(req.JSON))
	if err != nil {
		return nil, err
	}
	// 可选覆盖 machine/device（扁平 JSON 手建场景）
	if req.MachineID != "" {
		a.MachineID = req.MachineID
	}
	if req.DeviceID != "" {
		if !upstream.IsDecimalDeviceID(req.DeviceID) {
			return nil, fmt.Errorf("deviceId 必须为 12~20 位十进制数字（hex32/UUID 会被上游 9074 拒绝）")
		}
		a.DeviceID = req.DeviceID
	}
	if req.ProxyURL != "" {
		if !validProxyURL(strings.TrimSpace(req.ProxyURL)) {
			return nil, fmt.Errorf("proxy_url 需为 http/https/socks5 代理地址")
		}
		a.ProxyURL = strings.TrimSpace(req.ProxyURL)
	} else if a.ProxyURL == "" {
		h.autoAssignProxy(a) // 未指定出口时从代理池自动均衡分配
	}
	// 缺省 host/domain 补默认
	if a.Domain == "" {
		a.Domain = "trae.cn"
	}
	if a.ApiHost == "" {
		a.ApiHost = "https://api.trae.com.cn"
	}
	// 补设备身份（同 importFromCallback：十进制号，每账号独立）
	genDev := false
	if a.MachineID == "" {
		if mid, rerr := randomHex(16); rerr == nil {
			a.MachineID = mid
			genDev = true
		}
	}
	if !upstream.IsDecimalDeviceID(a.DeviceID) {
		if did, rerr := upstream.NewDeviceID(); rerr == nil {
			a.DeviceID = did
			genDev = true
		}
	}
	if genDev {
		log.Printf("import: uid=%s 已补齐设备身份（device 为本账号独立十进制号）", a.UID)
	}
	// 设备注册（同 importFromJSON）：导入即注册本账号专属设备，绕开新号 9074。
	if did, rerr := h.cfg.Upstream.RegisterDevice(a); rerr == nil && upstream.IsDecimalDeviceID(did) {
		a.DeviceID = did
		log.Printf("import: uid=%s 已注册独立设备号 %s", a.UID, did)
	} else if rerr != nil {
		log.Printf("import: uid=%s 设备注册失败（退回随机号）: %v", a.UID, rerr)
	}
	return a, nil
}

// adminDeleteAccount DELETE /admin/api/accounts/{uid}：删池条目 + 删 auths 文件。
func (h *Handler) adminDeleteAccount(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing uid")
		return
	}
	fp := auth.FilePathFor(h.cfg.AuthDir, uid)
	removed := h.cfg.Pool.Remove(uid)
	if !removed {
		// 池中无但有残留文件 → 也清掉文件（幂等）
		if _, err := os.Stat(fp); err == nil {
			_ = os.Remove(fp)
		}
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not in pool")
		return
	}
	_ = os.Remove(fp) // 池条目已删，文件尽量删（不存在不报错）
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "deleted": true})
}

// patchRequest PATCH 体：开关 / nickname。
type patchRequest struct {
	Enabled  *bool   `json:"enabled,omitempty"`
	Nickname *string `json:"nickname,omitempty"`
	ProxyURL *string `json:"proxy_url,omitempty"` // 原始地址（API 兼容）；"" = 清除
	Proxy    *string `json:"proxy,omitempty"`     // 代理池名称 / 原始地址 / "" = 清除（面板用）
}

// adminPatchAccount PATCH /admin/api/accounts/{uid}：软开关 / nickname。
// token 字段一律拒绝修改（脱敏预览不返回真值，PATCH 不接受 token）。
func (h *Handler) adminPatchAccount(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var req patchRequest
	if err := decodeBody(w, r, &req); err != nil {
		return
	}
	st, ok := h.cfg.Pool.Status(uid)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found")
		return
	}
	if req.Enabled != nil {
		reason := "user enabled"
		if !*req.Enabled {
			reason = "user disabled"
		}
		if !h.cfg.Pool.SetEnabled(uid, *req.Enabled, reason) {
			writeOpenAIError(w, http.StatusNotFound, "not_found", "account vanished")
			return
		}
	}
	_ = st // nickname 更新需要改 auth 文件
	if req.Nickname != nil && *req.Nickname != "" {
		a := h.cfg.Pool.AuthByUID(uid)
		if a == nil {
			writeOpenAIError(w, http.StatusNotFound, "not_found", "no auth for uid")
			return
		}
		a.Nickname = *req.Nickname
		if a.FilePath == "" {
			a.FilePath = auth.FilePathFor(h.cfg.AuthDir, uid)
		}
		_ = a.SaveAtomic()
	}
	if req.ProxyURL != nil {
		p := strings.TrimSpace(*req.ProxyURL)
		if p != "" && !validProxyURL(p) {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
				"proxy_url 需为 http/https/socks5 代理地址，如 socks5://user:pass@1.2.3.4:1080")
			return
		}
		a := h.cfg.Pool.AuthByUID(uid)
		if a == nil {
			writeOpenAIError(w, http.StatusNotFound, "not_found", "no auth for uid")
			return
		}
		a.ProxyURL = p
		if a.FilePath == "" {
			a.FilePath = auth.FilePathFor(h.cfg.AuthDir, uid)
		}
		if err := a.SaveAtomic(); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "save_failed", err.Error())
			return
		}
		log.Printf("proxy updated uid=%s -> %s", uid, maskProxy(p))
	}
	if req.Proxy != nil {
		// 面板路径：值可为代理池名称、原始代理地址或 ""（直连）。
		v := strings.TrimSpace(*req.Proxy)
		resolved := ""
		if v != "" {
			if u, ok := h.proxies.Resolve(v); ok {
				resolved = u
			} else if validProxyURL(v) {
				resolved = v
			} else {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
					"代理不存在（代理池里没有 "+v+"）且不是合法代理地址")
				return
			}
		}
		a := h.cfg.Pool.AuthByUID(uid)
		if a == nil {
			writeOpenAIError(w, http.StatusNotFound, "not_found", "no auth for uid")
			return
		}
		a.ProxyURL = resolved
		if a.FilePath == "" {
			a.FilePath = auth.FilePathFor(h.cfg.AuthDir, uid)
		}
		if err := a.SaveAtomic(); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "save_failed", err.Error())
			return
		}
		log.Printf("proxy updated uid=%s -> %s", uid, maskProxy(resolved))
	}
	st2, _ := h.cfg.Pool.Status(uid)
	writeJSON(w, http.StatusOK, st2)
}

// validProxyURL 校验代理地址 scheme。
func validProxyURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		return true
	}
	return false
}

// maskProxy 代理地址脱敏：隐藏 userinfo 凭据（scheme://***@host:port）。
func maskProxy(s string) string {
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil {
		return "(unparsable)"
	}
	if u.User != nil {
		return u.Scheme + "://***@" + u.Host
	}
	return u.Scheme + "://" + u.Host
}

// autoAssignProxy 未显式指定出口时，从代理池挑被占用最少的地址分配给账号
// （新账号自动均衡，避免都挤在同一个出口）。空池不动作。
func (h *Handler) autoAssignProxy(a *auth.Auth) {
	if a.ProxyURL != "" {
		return
	}
	inUse := map[string]int{}
	for _, st := range h.cfg.Pool.List() {
		if x := h.cfg.Pool.AuthByUID(st.UID); x != nil && x.ProxyURL != "" {
			inUse[x.ProxyURL]++
		}
	}
	if u, ok := h.proxies.LeastUsed(inUse); ok {
		a.ProxyURL = u
		log.Printf("import: uid=%s 自动分配代理出口 %s", a.UID, maskProxy(u))
	}
}

// adminRegisterDevice POST /admin/api/accounts/{uid}/device_register：
// 为存量账号向上游注册一台新设备并启用（修复 claim 9074「设备未注册」）。
func (h *Handler) adminRegisterDevice(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := h.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "no auth for uid")
		return
	}
	did, err := h.cfg.Upstream.RegisterDevice(a)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	old := a.DeviceID
	a.DeviceID = did
	if a.FilePath == "" {
		a.FilePath = auth.FilePathFor(h.cfg.AuthDir, uid)
	}
	if err := a.SaveAtomic(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "save: " + err.Error()})
		return
	}
	log.Printf("device registered uid=%s: %s → %s", uid, old, did)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "device_id": did, "old": old})
}

// adminProxyTest POST /admin/api/accounts/{uid}/proxy_test：用该账号当前
// 代理出口实调一次额度接口，验证代理可用性与延迟。
func (h *Handler) adminProxyTest(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := h.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "no auth for uid")
		return
	}
	start := time.Now()
	packs, remain, _, _, err := h.cfg.Upstream.EntUsageDetail(a)
	lat := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok": false, "proxy": maskProxy(a.ProxyURL), "latency_ms": lat, "error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "proxy": maskProxy(a.ProxyURL), "latency_ms": lat,
		"remain": remain, "packs": len(packs),
	})
}

// adminRefreshAccount POST /admin/api/accounts/{uid}/refresh：手动 ExchangeToken + 落盘。
func (h *Handler) adminRefreshAccount(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := h.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "no auth for uid")
		return
	}
	if err := h.cfg.Upstream.RefreshToken(a); err != nil {
		// session 失效 → 硬禁用（与 chat 路径一致）
		if isSessionDeadErr(err) {
			h.cfg.Pool.Disable(uid, "manual refresh: session dead")
		}
		writeOpenAIError(w, http.StatusBadGateway, "refresh_failed", err.Error())
		return
	}
	if a.FilePath == "" {
		a.FilePath = auth.FilePathFor(h.cfg.AuthDir, uid)
	}
	if err := a.SaveAtomic(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	st, _ := h.cfg.Pool.Status(uid)
	writeJSON(w, http.StatusOK, map[string]any{
		"uid":       uid,
		"expires_at": a.ExpiresAt,
		"status":    st,
	})
}

// adminAccountJSON GET /admin/api/accounts/{uid}/json：脱敏预览（只读，无鉴权但严格脱敏）。
func (h *Handler) adminAccountJSON(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := h.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "no auth for uid")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uid":           a.UID,
		"nickname":      a.Nickname,
		"enterprise_id": a.EnterpriseID,
		"domain":        a.Domain,
		"api_host":      a.ApiHost,
		"machine_id":    prefix(a.MachineID, 8),
		"device_id":     prefix(a.DeviceID, 8),
		"access_token":  auth.MaskToken(a.JWT(), 12),
		"refresh_token": auth.MaskToken(a.RefreshTokenValue(), 12),
		"expires_at":    a.ExpiresAt,
		"file_path":     a.FilePath,
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// decodeBody 限长读 + JSON 解码，失败写 400 并返回 err。
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse json: "+err.Error())
		return err
	}
	return nil
}

// prefix 返回 s 前 n 字符（脱敏 machine/device id 展示）。
func prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// isSessionDeadErr 粗判 refresh 失败是否 session 失效（含 401/invalid token 标记）。
// 简化判断：错误信息含 session/unauthorized/401/invalid token。
func isSessionDeadErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, m := range []string{"session", "unauthorized", "401", "invalid token", "token 失效"} {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}
