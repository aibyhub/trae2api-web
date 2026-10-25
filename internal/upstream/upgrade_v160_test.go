// upgrade_v160_test.go 2026-10 客户端 1.107.1 对齐升级的回归测试：
// 菜单 batch 端点回退、追踪头、PKCE、ExchangeToken v3 设备证明、reasoning 透传。
package upstream

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"trae2api-web/internal/auth"
)

// --- P1-2 batch_get_detail_param（真实 camelCase 协议，2026-10-25 生产验证）---

func TestFetchModelsBatchFallback(t *testing.T) {
	calls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		switch r.URL.Path {
		case EpModelsBatch:
			return jsonResp(500, `{"error":"batch down"}`), nil
		case EpModels:
			return jsonResp(200, `{"config_info_list":[{"config_name":"glm-5.2","display_config":{"display_name":"GLM 5.2"}}]}`), nil
		}
		return nil, fmt.Errorf("unexpected path %s", r.URL.Path)
	})
	a := &auth.Auth{AccessToken: "at"}
	infos, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("fallback fetch failed: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls (batch+legacy), got %d", calls)
	}
	if len(infos) != 1 || infos[0].ID != "glm-5.2" {
		t.Fatalf("unexpected infos: %+v", infos)
	}
}

func TestFetchModelsBatchSuccess(t *testing.T) {
	var gotBody map[string]any
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != EpModelsBatch {
			return nil, fmt.Errorf("expected batch path, got %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &gotBody)
		// 真机协议：serde camelCase + PascalCase 枚举字符串
		if gotBody["modeType"] != "Manual" || gotBody["accessType"] != "SoloLite" {
			t.Errorf("batch body not real-machine camelCase form: %s", raw)
		}
		fns, _ := gotBody["functions"].([]any)
		if len(fns) != 1 || fns[0] != Function {
			t.Errorf("functions wrong: %v", fns)
		}
		return jsonResp(200, `{"function_configs":[{"function":"solo_work_lite","config_info_list":[
			{"config_name":"glm-5.3","display_config":{"display_name":"GLM 5.3"},"context_window_tokens":{"dev":200000},"display_contact_config":"{\"consumption_rate\":{\"enable\":true,\"data\":{\"rate\":0.9}}}"},
			{"config_name":"kimi-k3","display_config":{"display_name":"Kimi K3"},"context_window_size":131072}
		]}]}`), nil
	})
	a := &auth.Auth{AccessToken: "at"}
	infos, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("batch fetch failed: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("unexpected infos: %+v", infos)
	}
	if infos[0].ID != "glm-5.3" || infos[0].ContextWindow != 200000 || infos[0].Rate != 0.9 {
		t.Fatalf("glm-5.3 parse wrong: %+v", infos[0])
	}
	if infos[1].ID != "kimi-k3" || infos[1].ContextWindow != 131072 {
		t.Fatalf("kimi-k3 parse wrong: %+v", infos[1])
	}
}

// --- P1-4 x-custom-trace-id / x-flow-traceparent（真机形态，2026-10-25 对齐）---

func TestSoloTraceHeaders(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://agent.example/x", nil)
	SOLOHeaders(req, &auth.Auth{AccessToken: "at"}, false)
	trace := req.Header.Get("X-Custom-Trace-Id")
	tp := req.Header.Get("X-Flow-Traceparent")
	if len(trace) != 32 {
		t.Fatalf("x-custom-trace-id not hex32: %q", trace)
	}
	// 真机形态：04-<trace32>-<span16>-01（非 W3C 标准 00 前缀）
	if !strings.HasPrefix(tp, "04-"+trace+"-") || !strings.HasSuffix(tp, "-01") {
		t.Fatalf("x-flow-traceparent wrong form: %q (trace=%s)", tp, trace)
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(tp, "04-"+trace+"-"), "-01")
	if len(mid) != 16 {
		t.Fatalf("span not hex16: %q", mid)
	}
	if req.Header.Get("traceparent") != "" {
		t.Fatal("standard traceparent must not be sent (真机不发)")
	}
}

// --- PKCE（server 包的 pkcePair 在 server 包测；这里测真机同构的 S256 关系由
// server 包 callback/login 测试覆盖） ---

// --- P3-9 ExchangeToken v3 DeviceProof ---

func TestDeviceCredentialsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/device-key.json"
	dc1, err := LoadOrCreateDeviceCredentials(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	dc2, err := LoadOrCreateDeviceCredentials(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if dc1.PrivateKeyPEM != dc2.PrivateKeyPEM || dc1.PublicKeyPEM != dc2.PublicKeyPEM {
		t.Fatal("device key not stable across reload")
	}
}

func TestSignDeviceProofVerifiable(t *testing.T) {
	dc, err := generateDeviceCredentials()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	proof, err := dc.SignDeviceProof("POST", EpExchangeV3, ClientID, "refresh-token-1")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if proof.Timestamp <= 0 || len(proof.Nonce) != 32 {
		t.Fatalf("bad proof meta: %+v", proof)
	}
	// 复算 canonical 并用公钥验签（对齐真机 eTe() 语义）。
	canonical := "POST\n" + EpExchangeV3 + "\n" + ClientID + "\nrefresh-token-1\n" +
		fmt.Sprintf("%d", proof.Timestamp) + "\n" + proof.Nonce
	sig, err := base64.StdEncoding.DecodeString(proof.Signature)
	if err != nil {
		t.Fatalf("signature not base64: %v", err)
	}
	// 用公钥重建 ecdsa.PublicKey 验签
	parsed, err := parsePEMPubKey(dc.PublicKeyPEM)
	if err != nil {
		t.Fatalf("parse pub: %v", err)
	}
	digest := sha256.Sum256([]byte(canonical))
	if !ecdsa.VerifyASN1(parsed, digest[:], sig) {
		t.Fatal("signature does not verify against canonical string")
	}
	// canonical 任一成分不同 → 验签必须失败
	wrong := "POST\n" + EpExchangeV3 + "\n" + ClientID + "\nother-token\n" +
		fmt.Sprintf("%d", proof.Timestamp) + "\n" + proof.Nonce
	dw := sha256.Sum256([]byte(wrong))
	if ecdsa.VerifyASN1(parsed, dw[:], sig) {
		t.Fatal("signature verified against wrong canonical")
	}
}

func TestUseExchangeV3DefaultOff(t *testing.T) {
	// 环境未设置 TW2A_EXCHANGE_V3（测试进程内 Once 首次读即默认关）：
	// 即使设备密钥已装配也不走 v3 —— 生产默认行为零变化。
	c := testClient(func(r *http.Request) (*http.Response, error) { return nil, errors.New("no io") })
	c.DeviceCreds, _ = generateDeviceCredentials()
	if c.useExchangeV3() {
		t.Fatal("exchange v3 must default to OFF")
	}
}

// TestExchangeV3FallsBackToLegacy v3 开启但上游拒绝（如签名校验失败 401）时，
// 必须**同一次刷新内**回退 legacy 拿到新 token——这是 TW2A_EXCHANGE_V3 零风险
// 切换的兜底保证（2026-10-25 加固）。
func TestExchangeV3FallsBackToLegacy(t *testing.T) {
	exchangeV3Enabled() // 确保 Once 已读（测试进程 env 未设 → 默认 false）
	defer func() { exchangeV3Val = false }()
	exchangeV3Val = true // 白盒翻转开关（同包测试）

	var calls []string
	var v3Headers http.Header
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.URL.Host+r.URL.Path)
		switch r.URL.Path {
		case EpExchangeV3:
			v3Headers = r.Header.Clone()
			return jsonResp(401, `{"ResponseMetadata":{"Error":{"Code":"20101","Message":"Token 无效"}}}`), nil
		case EpExchange:
			return jsonResp(200, `{"Result":{"Token":"new-at","RefreshToken":"new-rt","TokenExpireDuration":86400}}`), nil
		}
		return nil, fmt.Errorf("unexpected path %s", r.URL.Path)
	})
	dc, _ := generateDeviceCredentials()
	c.DeviceCreds = dc
	a := &auth.Auth{RefreshToken: "old-rt", AccessToken: "old-at"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh should succeed via legacy fallback: %v", err)
	}
	if len(calls) != 2 || calls[0] != "ug.example"+EpExchangeV3 || calls[1] != "oauth.example"+EpExchange {
		t.Fatalf("expected v3(ug.example)-then-legacy(oauth.example) calls, got %v", calls)
	}
	// 真机抓包对齐的头：旧 access token 随 X-Cloudide-Token 携带
	if v3Headers.Get("X-Cloudide-Token") != "old-at" {
		t.Fatalf("v3 request missing X-Cloudide-Token: %v", v3Headers)
	}
	if v3Headers.Get("X-Request-Id") == "" || v3Headers.Get("X-Tt-Trace-Id") == "" {
		t.Fatalf("v3 request missing trace headers: %v", v3Headers)
	}
	if a.AccessToken != "new-at" || a.RefreshToken != "new-rt" {
		t.Fatalf("token not updated by fallback: %+v", a)
	}
}

func TestBuildDeviceInfoShape(t *testing.T) {
	dc, _ := generateDeviceCredentials()
	info := BuildDeviceInfo(&auth.Auth{DeviceID: "123", MachineID: "abc"}, dc)
	if info["DeviceID"] != "123" || info["MachineID"] != "abc" {
		t.Fatalf("device ids missing: %+v", info)
	}
	if info["PlatformCode"] != "SOLO_PC" || info["DeviceType"] != "PC" {
		t.Fatalf("platform fields wrong: %+v", info)
	}
	if info["DevicePublicKey"] != dc.PublicKeyPEM {
		t.Fatal("DevicePublicKey missing")
	}
	raw, _ := json.Marshal(info)
	if !strings.Contains(string(raw), "DevicePublicKey") {
		t.Fatal("device public key not serialized")
	}
}

// --- P2-6 reasoning_effort 透传 ---

func TestPrepareBodyReasoningEffortPassthrough(t *testing.T) {
	src := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high","temperature":0.7}`)
	out := PrepareBody(src)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("re-marshal broken: %v", err)
	}
	if obj["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort lost: %v", obj["reasoning_effort"])
	}
	if obj["temperature"] != 0.7 {
		t.Fatalf("temperature lost: %v", obj["temperature"])
	}
	if obj["function"] != Function || obj["config_name"] != "glm-5.2" {
		t.Fatalf("core fields wrong: function=%v config=%v", obj["function"], obj["config_name"])
	}
}

// --- P1-2 ChatStream context 取消止损 ---

func TestChatStreamContextCancel(t *testing.T) {
	// 客户端断开（ctx 取消）→ 上游请求同步终止，不再陪跑完整生成。
	// mock 传输需自观察 r.Context()（与真实 net/http.Transport 语义一致）。
	block := make(chan struct{})
	c := testClient(func(r *http.Request) (*http.Response, error) {
		select {
		case <-block:
			return jsonResp(200, `{"ok":true}`), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, _, err := c.ChatStream(ctx, a, []byte(`{}`))
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled ctx should abort the upstream request with an error")
		}
		if !strings.Contains(err.Error(), context.Canceled.Error()) {
			t.Fatalf("error not context.Canceled: %v", err)
		}
	case <-time.After(3 * time.Second):
		close(block)
		t.Fatal("ChatStream did not return after ctx cancel")
	}
}

// TestExchangeV3SchemaMismatchFallsBack v3 返回 HTTP 200 但缺 Result.Token（schema 意外）
// 时，必须同样回退 legacy——不留半更新、不让 refresh 整体失败（2026-10-25 加固）。
func TestExchangeV3SchemaMismatchFallsBack(t *testing.T) {
	exchangeV3Enabled()
	defer func() { exchangeV3Val = false }()
	exchangeV3Val = true

	var calls []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.URL.Path)
		switch r.URL.Path {
		case EpExchangeV3:
			return jsonResp(200, `{"Result":{"Unrelated":"field"}}`), nil
		case EpExchange:
			return jsonResp(200, `{"Result":{"Token":"legacy-at","RefreshToken":"legacy-rt","TokenExpireDuration":86400}}`), nil
		}
		return nil, fmt.Errorf("unexpected path %s", r.URL.Path)
	})
	dc, _ := generateDeviceCredentials()
	c.DeviceCreds = dc
	a := &auth.Auth{RefreshToken: "old-rt", AccessToken: "old-at"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh should succeed via legacy fallback: %v", err)
	}
	if len(calls) != 2 || calls[0] != EpExchangeV3 || calls[1] != EpExchange {
		t.Fatalf("expected v3-then-legacy calls, got %v", calls)
	}
	if a.AccessToken != "legacy-at" {
		t.Fatalf("legacy token not applied: %+v", a)
	}
}

// --- P1-1 clientFor 畸形代理 URL 不再 panic ---

func TestClientForMalformedProxyNoPanic(t *testing.T) {
	// url.Parse 失败时返回 nil URL——旧行为在日志行解引用 u.Scheme 直接 panic
	// （且 scheduler goroutine 无 recover 会杀进程）。修复后必须回退直连。
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", ProxyURL: "http://[::1"}
	std, stream := c.clientFor(a)
	if std == nil || stream == nil {
		t.Fatal("malformed proxy must fall back to DIRECT clients, not panic")
	}
	// 同一畸形 URL 走缓存，二次调用同样安全
	if _, stream2 := c.clientFor(a); stream2 == nil {
		t.Fatal("cached malformed proxy entry lost")
	}
}

// --- 刷新非阻塞化（2026-10-25 热路径改造）---

// TestRefreshIfNeededConcurrentSingleFlight 并发预刷新：网络只发一次，
// 其余调用立即 (false,nil) 不排队——写锁队头阻塞问题的行为锚定。
func TestRefreshIfNeededConcurrentSingleFlight(t *testing.T) {
	var calls int32
	gate := make(chan struct{})
	c := testClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		<-gate // 第一次网络挂起，窗口内其余调用必须走「在飞 → (false,nil)」
		return jsonResp(200, `{"Result":{"Token":"new-at","RefreshToken":"new-rt","TokenExpireDuration":86400}}`), nil
	})
	a := &auth.Auth{RefreshToken: "old-rt", AccessToken: "old-at", ExpiresAt: time.Now().Unix()} // 已过期
	const n = 8
	type res struct {
		refreshed bool
		err       error
	}
	results := make(chan res, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			refreshed, err := c.RefreshTokenIfNeeded(a, 24*time.Hour)
			results <- res{refreshed, err}
		}()
	}
	time.Sleep(100 * time.Millisecond) // 让 first 请求挂住、其余人判定「在飞」
	close(gate)
	wg.Wait()
	close(results)
	total := 0
	for r := range results {
		if r.err != nil {
			t.Fatalf("no caller should error: %v", r.err)
		}
		total++
	}
	if int32(total) != n {
		t.Fatalf("expected %d results, got %d", n, total)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("exchange should fire exactly once, got %d", got)
	}
	if a.AccessToken != "new-at" || a.RefreshToken != "new-rt" {
		t.Fatalf("token not applied: %+v", a)
	}
}

// TestCompleteRefreshCASDiscardStale 并发轮抢先轮换后，慢轮结果必须丢弃
// （不允许旧快照覆盖新 token），且 refreshing 标记被清。
func TestCompleteRefreshCASDiscardStale(t *testing.T) {
	a := &auth.Auth{RefreshToken: "rt-v1", AccessToken: "at-v1"}
	if _, ok := a.BeginRefresh(24 * time.Hour); !ok {
		t.Fatal("begin failed")
	}
	// 模拟并发轮已用 rt-v1 换到新 token 并写回
	a.CompleteRefresh("rt-v1", "at-v2", "rt-v2", time.Now().Add(time.Hour).Unix())
	// 慢轮（拿着 rt-v1 的结果）迟到 → 必须被丢弃
	a.CompleteRefresh("rt-v1", "at-SLOW", "rt-SLOW", 0)
	if a.AccessToken != "at-v2" || a.RefreshToken != "rt-v2" {
		t.Fatalf("stale result overwrote fresh tokens: %+v", a)
	}
	// 标记已清（可以直接再次 BeginRefresh）
	if _, ok := a.BeginRefresh(24 * time.Hour); !ok {
		t.Fatal("refreshing flag not cleared after CAS discard")
	}
}

// TestBeginRefreshSkipsWhenFresh token 新鲜时不标记不刷新。
func TestBeginRefreshSkipsWhenFresh(t *testing.T) {
	a := &auth.Auth{RefreshToken: "rt", ExpiresAt: time.Now().Add(48 * time.Hour).Unix()}
	if _, ok := a.BeginRefresh(24 * time.Hour); ok {
		t.Fatal("fresh token should not begin refresh")
	}
	if _, ok := a.BeginRefresh(24 * time.Hour); ok {
		t.Fatal("second begin should also skip")
	}
}

// --- Classify 报文体长度门 + doJSON 读错误归类（2026-10-25 稳定性收紧）---

func TestClassifyPlanLimitSizeGate(t *testing.T) {
	// 小报文：宽松路径仍生效（历史无 "code":1005 锚点的形态）
	small := `{"message":"plan quota 1005 exceeded"}`
	if got := Classify(200, small); got != ErrPlanLimit {
		t.Fatalf("small plan body should classify plan_limit, got %v", got)
	}
	// 大报文（5xx 回显用户内容，恰好含 "1005" 与 "plan" 子串）→ 不再误判 12h 硬冷却
	big := `{"error":"boom","echo":"` + strings.Repeat("x plan 1005 ", 200) + `"}`
	if got := Classify(500, big); got == ErrPlanLimit {
		t.Fatal("large echoed body must not classify plan_limit")
	}
	if got := Classify(500, big); got != ErrServer {
		t.Fatalf("large body should stay ErrServer, got %v", got)
	}
	// 精确锚不受长度影响
	if got := Classify(200, `{"code":1005,"msg":"x"}`); got != ErrPlanLimit {
		t.Fatalf("exact anchor broken: %v", got)
	}
}

// TestDoJSONBodyReadErrorIsTransport 响应体中途断流 → 传输层错误（出口隔离路径），
// 不再变成 "exchange parse" 类账号错误。
func TestDoJSONBodyReadErrorIsTransport(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(&errReader{}),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	err := c.CheckinClaim(a)
	if err == nil {
		t.Fatal("mid-body read error should surface")
	}
	if !IsTransportError(err) {
		t.Fatalf("read error should classify as transport: %v", err)
	}
}

type errReader struct{}

func (errReader) Read(p []byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (errReader) Close() error               { return nil }
