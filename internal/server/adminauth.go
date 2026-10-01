// adminauth.go 面板登录（可选，内置简单登录页，可替代 Nginx Basic Auth）。
//
// 设了 TW2A_ADMIN_PASSWORD 后：
//   - /admin 仍返回页面（前端弹登录框），但所有 /admin/api/* 读写都要先登录；
//   - 会话是 HMAC 签名的无状态 Cookie（默认 7 天），密码只存环境变量、不落盘不进日志；
//   - 同时保留 Bearer API Key 通道（脚本/curl 不受影响），两者任一有效即可（OR）。
//
// 没设密码时行为与历史一致（只读接口开放、写接口要 API Key）。
package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	adminCookieName = "tw2a_admin"
	adminSessionTTL = 7 * 24 * time.Hour
	adminMaxFails   = 10
	adminFailWindow = 10 * time.Minute
)

// adminAuth 登录失败计数（进程内，按来源 IP 简单节流；单管理员面板足够）。
type adminAuth struct {
	mu    sync.Mutex
	fails map[string][]time.Time
}

func (a *adminAuth) noteFail(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fails == nil {
		a.fails = map[string][]time.Time{}
	}
	now := time.Now()
	keep := make([]time.Time, 0, len(a.fails[ip])+1)
	for _, t := range a.fails[ip] {
		if now.Sub(t) < adminFailWindow {
			keep = append(keep, t)
		}
	}
	a.fails[ip] = append(keep, now)
}

func (a *adminAuth) blocked(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	n := 0
	for _, t := range a.fails[ip] {
		if now.Sub(t) < adminFailWindow {
			n++
		}
	}
	return n >= adminMaxFails
}

func (a *adminAuth) clear(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.fails, ip)
}

// adminSecret 由密码派生签名密钥：进程重启后已发出的 Cookie 依然有效。
func (h *Handler) adminSecret() []byte {
	sum := sha256.Sum256([]byte("trae2api-web/admin/v1/" + h.cfg.AdminPassword))
	return sum[:]
}

// issueAdminToken 生成 `exp.hex(hmac)` 形式会话令牌。
func (h *Handler) issueAdminToken(now time.Time) string {
	exp := now.Add(adminSessionTTL).Unix()
	mac := hmac.New(sha256.New, h.adminSecret())
	fmt.Fprintf(mac, "%d", exp)
	return strconv.FormatInt(exp, 10) + "." + hex.EncodeToString(mac.Sum(nil))
}

// adminTokenOK 校验会话令牌（过期 + 签名，常量时间比较）。
func (h *Handler) adminTokenOK(tok string) bool {
	i := strings.Index(tok, ".")
	if i <= 0 {
		return false
	}
	exp, err := strconv.ParseInt(tok[:i], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	mac := hmac.New(sha256.New, h.adminSecret())
	fmt.Fprintf(mac, "%d", exp)
	return hmac.Equal([]byte(tok[i+1:]), []byte(hex.EncodeToString(mac.Sum(nil))))
}

// bearerOK 校验 Authorization: Bearer <TW2A_API_KEY>（常量时间）。
func (h *Handler) bearerOK(r *http.Request) bool {
	if h.cfg.APIKey == "" {
		return false
	}
	authz := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(authz) < len(prefix) || !strings.EqualFold(authz[:len(prefix)], prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(authz[len(prefix):]), []byte(h.cfg.APIKey)) == 1
}

// sessionOK 会话 Cookie 有效。
func (h *Handler) sessionOK(r *http.Request) bool {
	c, err := r.Cookie(adminCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	return h.adminTokenOK(c.Value)
}

// adminAuthorized 会话 Cookie 或 Bearer API Key 任一有效即可。
func (h *Handler) adminAuthorized(r *http.Request) bool {
	return h.sessionOK(r) || h.bearerOK(r)
}

// authDisabled 密码与 API Key 都没配（本地裸用）时不鉴权，保持历史行为。
func (h *Handler) authDisabled() bool {
	return h.cfg.AdminPassword == "" && h.cfg.APIKey == ""
}

// adminGate 面板总闸：配了密码时 /admin/api/* 一律要登录（读 + 写）。
// /admin 页面本身不拦（前端弹登录框），/authorize 等非面板路径也不拦。
func (h *Handler) adminGate(r *http.Request) bool {
	if h.cfg.AdminPassword == "" {
		return true
	}
	if !strings.HasPrefix(r.URL.Path, "/admin/api/") {
		return true
	}
	return h.adminAuthorized(r)
}

// clientIP 取来源 IP（仅用 RemoteAddr，避免可伪造的 X-Forwarded-For 影响节流）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// adminSessionGet GET /admin/session：面板启动时探测登录态（本身不鉴权）。
func (h *Handler) adminSessionGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_required": h.cfg.AdminPassword != "",
		"authenticated": h.adminAuthorized(r),
		"has_api_key":   h.cfg.APIKey != "",
	})
}

// adminLogin POST /admin/login {password}：校验密码并下发会话 Cookie。
func (h *Handler) adminLogin(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AdminPassword == "" {
		writeOpenAIError(w, http.StatusBadRequest, "no_password", "未配置 TW2A_ADMIN_PASSWORD，无需登录")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16)).Decode(&req)
	ip := clientIP(r)
	if h.admin.blocked(ip) {
		writeOpenAIError(w, http.StatusTooManyRequests, "too_many_attempts", "尝试过于频繁，请 10 分钟后再试")
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Password), []byte(h.cfg.AdminPassword)) != 1 {
		h.admin.noteFail(ip)
		time.Sleep(400 * time.Millisecond) // 失败节流
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_password", "密码不正确")
		return
	}
	h.admin.clear(ip)
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    h.issueAdminToken(time.Now()),
		Path:     "/",
		MaxAge:   int(adminSessionTTL / time.Second),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// adminLogout POST /admin/logout：清掉会话 Cookie。
func (h *Handler) adminLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
