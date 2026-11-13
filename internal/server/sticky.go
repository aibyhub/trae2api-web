package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const stickySeedLimit = 8 << 10

// peekMessage 仅取会话指纹所需的字段（正文按 RawMessage 原样哈希）。
type peekMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// conversationKey 派生会话粘性键：model + user + 首条非 system 消息的前 8KB。
// 同一会话后续请求会携带完整历史，首条用户消息不变 → 键稳定；不同会话/不同模型
// 自然分开。new-api 不转发 X-Token-Name，因此不能依赖请求头。
func conversationKey(model, user string, msgs []peekMessage) string {
	var seed []byte
	for _, m := range msgs {
		if m.Role == "" || m.Role == "system" {
			continue
		}
		c := bytes.TrimSpace(m.Content)
		if len(c) == 0 {
			continue
		}
		if len(c) > stickySeedLimit {
			c = c[:stickySeedLimit]
		}
		seed = append(seed, m.Role...)
		seed = append(seed, 0)
		seed = append(seed, c...)
		break
	}
	if len(seed) == 0 {
		return ""
	}
	h := sha256.New()
	h.Write([]byte("trae-sticky-v1"))
	h.Write([]byte{0})
	h.Write([]byte(model))
	h.Write([]byte{0})
	h.Write([]byte(user))
	h.Write([]byte{0})
	h.Write(seed)
	return hex.EncodeToString(h.Sum(nil)[:16])
}
