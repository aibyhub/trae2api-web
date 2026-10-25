// upgrade_v160_test.go 2026-10 客户端 1.107.1 对齐升级的回归测试：
// 菜单 batch 端点回退、追踪头、PKCE、ExchangeToken v3 设备证明、reasoning 透传。
package upstream

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

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
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.URL.Path)
		switch r.URL.Path {
		case EpExchangeV3:
			return jsonResp(401, `{"ResponseMetadata":{"Error":{"Code":"20101","Message":"Token 无效"}}}`), nil
		case EpExchange:
			return jsonResp(200, `{"Result":{"Token":"new-at","RefreshToken":"new-rt","TokenExpireDuration":86400}}`), nil
		}
		return nil, fmt.Errorf("unexpected path %s", r.URL.Path)
	})
	dc, _ := generateDeviceCredentials()
	c.DeviceCreds = dc
	a := &auth.Auth{RefreshToken: "old-rt"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh should succeed via legacy fallback: %v", err)
	}
	if len(calls) != 2 || calls[0] != EpExchangeV3 || calls[1] != EpExchange {
		t.Fatalf("expected v3-then-legacy calls, got %v", calls)
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
