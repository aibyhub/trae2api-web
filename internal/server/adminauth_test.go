// adminauth_test.go 面板内置登录（可选）的行为回归。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"trae2api-web/internal/pool"
)

func newAuthHandler(password, apiKey string) *Handler {
	return NewHandler(Config{Pool: pool.New(""), AdminPassword: password, APIKey: apiKey})
}

func doReq(h *Handler, method, path, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAdminLoginGate 设了密码后：读+写都要登录；登录页与会话探测本身放行。
func TestAdminLoginGate(t *testing.T) {
	h := newAuthHandler("s3cret", "")

	if rec := doReq(h, "GET", "/admin/api/accounts", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth read = %d want 401", rec.Code)
	}
	if rec := doReq(h, "GET", "/admin", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("admin page = %d want 200（登录页要能打开）", rec.Code)
	}
	rec := doReq(h, "GET", "/admin/session", "", nil)
	if !strings.Contains(rec.Body.String(), "auth_required") || !strings.Contains(rec.Body.String(), "true") {
		t.Fatalf("session body = %s", rec.Body.String())
	}
	if rec := doReq(h, "POST", "/admin/login", "{\"password\":\"nope\"}", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d want 401", rec.Code)
	}

	rec = doReq(h, "POST", "/admin/login", "{\"password\":\"s3cret\"}", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login must set a session cookie")
	}
	if rec := doReq(h, "GET", "/admin/api/accounts", "", func(r *http.Request) {
		r.AddCookie(cookies[0])
	}); rec.Code != http.StatusOK {
		t.Fatalf("authenticated read = %d want 200", rec.Code)
	}
	if rec := doReq(h, "POST", "/admin/api/checkin_all", "", func(r *http.Request) {
		r.AddCookie(cookies[0])
	}); rec.Code == http.StatusUnauthorized {
		t.Fatal("authenticated write must not be 401")
	}
}

// TestAdminBearerAndLegacy Bearer 通道保留；未设密码时行为与历史一致。
func TestAdminBearerAndLegacy(t *testing.T) {
	h := newAuthHandler("pw", "key123")
	if rec := doReq(h, "GET", "/admin/api/accounts", "", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer key123")
	}); rec.Code != http.StatusOK {
		t.Fatalf("bearer read = %d want 200", rec.Code)
	}
	if rec := doReq(h, "GET", "/admin/api/accounts", "", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer wrong")
	}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad bearer = %d want 401", rec.Code)
	}

	legacy := newAuthHandler("", "key123")
	if rec := doReq(legacy, "GET", "/admin/api/accounts", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("legacy read = %d want 200（不设密码时只读仍开放）", rec.Code)
	}
	if rec := doReq(legacy, "DELETE", "/admin/api/accounts/x", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("legacy write without key = %d want 401", rec.Code)
	}
}
