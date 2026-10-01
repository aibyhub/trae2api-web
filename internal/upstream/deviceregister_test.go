// deviceregister_test.go applog 封装修复性测试：加解密往返 + 头格式 + 指纹形态。
package upstream

import (
	"bytes"
	"testing"
)

func TestApplogDecorateRoundtrip(t *testing.T) {
	payload := []byte(`{"header":{"device_id":0},"magic_tag":"ss_app_log"}`)
	out, err := applogDecorate(payload)
	if err != nil {
		t.Fatal(err)
	}
	// 头 6 字节魔数 + 32 字节密钥
	if out[0] != 0x74 || out[1] != 0x63 || out[2] != 0x05 || out[3] != 0x10 || out[4] != 0x00 || out[5] != 0x00 {
		t.Fatalf("bad magic header: % x", out[:6])
	}
	if len(out) <= 6+32 {
		t.Fatal("payload too short")
	}
	// 解密还原（applogDecrypt 内部校验 SHA512 前缀，不符即报错）
	decoded, err := applogDecrypt(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("roundtrip mismatch:\n got %q\nwant %q", decoded, payload)
	}
	// 两次加密密钥不同 → 密文不同（随机 key）
	out2, _ := applogDecorate(payload)
	if bytes.Equal(out, out2) {
		t.Fatal("expected different ciphertext for different random keys")
	}
}

func TestGenDRFingerprintShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		fp := genDRFingerprint()
		if len(fp.UUID) != 36 || fp.UUID[8] != '-' {
			t.Fatalf("uuid shape: %q", fp.UUID)
		}
		if len(fp.MAC) != 17 || fp.MAC[2] != ':' {
			t.Fatalf("mac shape: %q", fp.MAC)
		}
		if fp.Model == "" || fp.Resolution == "" || fp.Serial == "" {
			t.Fatalf("empty fingerprint field: %+v", fp)
		}
		seen[fp.UUID] = true
	}
	if len(seen) != 50 {
		t.Fatalf("uuid collision: %d unique of 50", len(seen))
	}
}
