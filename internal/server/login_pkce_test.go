// login_pkce_test.go PKCE 生成与登录 URL 集成（2026-10 真机 1.107.1 对齐）。
package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"trae2api-web/internal/upstream"
)

func TestPKCEPairShape(t *testing.T) {
	verifier, challenge, err := pkcePair()
	if err != nil {
		t.Fatalf("pkcePair: %v", err)
	}
	// 真机 QDe()：verifier = 48 随机字节 base64url（64 字符），challenge = S256(verifier) base64url。
	if len(verifier) != 64 {
		t.Fatalf("verifier len=%d want 64 (48 bytes base64url)", len(verifier))
	}
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if challenge != want {
		t.Fatalf("challenge mismatch S256(verifier): got %s want %s", challenge, want)
	}
	// 两次生成互不相同（随机性）
	v2, c2, err := pkcePair()
	if err != nil {
		t.Fatalf("pkcePair 2: %v", err)
	}
	if v2 == verifier || c2 == challenge {
		t.Fatal("pkce pair not random")
	}
}

// TestAdminLoginStartIncludesPKCE 面板发起登录：URL 带 PKCE + tronBuildVersion。
func TestAdminLoginStartIncludesPKCE(t *testing.T) {
	h := newAuthHandler("", "test-key")
	mutate := func(r *http.Request) { r.Header.Set("Authorization", "Bearer test-key") }
	rec := doReq(h, "POST", "/admin/api/login", `{}`, mutate)
	if rec.Code != 200 {
		t.Fatalf("login start status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		LoginURL string `json:"login_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.LoginURL == "" {
		t.Fatalf("bad response: %s", rec.Body.String())
	}
	u, err := url.Parse(resp.LoginURL)
	if err != nil {
		t.Fatalf("parse login url: %v", err)
	}
	q := u.Query()
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("login url missing pkce params: %s", resp.LoginURL)
	}
	if q.Get("plugin_version") != upstream.TronBuildVersion {
		t.Fatalf("plugin_version=%q want %q", q.Get("plugin_version"), upstream.TronBuildVersion)
	}
	if q.Get("hide_saas_login") != "true" {
		t.Fatalf("hide_saas_login=%q want true", q.Get("hide_saas_login"))
	}
}
