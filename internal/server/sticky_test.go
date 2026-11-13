package server

import "testing"

func TestConversationKeyStable(t *testing.T) {
	msgs := []peekMessage{
		{Role: "system", Content: []byte(`"sys"`)},
		{Role: "user", Content: []byte(`"hello"`)},
		{Role: "assistant", Content: []byte(`"hi"`)},
	}
	a := conversationKey("deepseek-v4.1-flash", "u1", msgs)
	if a == "" {
		t.Fatal("key should not be empty")
	}
	// 追加后续对话不应改变键（首条用户消息不变）。
	msgs2 := append(msgs, peekMessage{Role: "user", Content: []byte(`"next"`)})
	if b := conversationKey("deepseek-v4.1-flash", "u1", msgs2); b != a {
		t.Fatalf("key changed with appended history: %s != %s", b, a)
	}
}

func TestConversationKeyScope(t *testing.T) {
	msgs := []peekMessage{{Role: "user", Content: []byte(`"hello"`)}}
	base := conversationKey("m1", "u1", msgs)
	if got := conversationKey("m2", "u1", msgs); got == base {
		t.Fatal("different model must produce different key")
	}
	if got := conversationKey("m1", "u2", msgs); got == base {
		t.Fatal("different user must produce different key")
	}
	other := []peekMessage{{Role: "user", Content: []byte(`"hello there"`)}}
	if got := conversationKey("m1", "u1", other); got == base {
		t.Fatal("different first message must produce different key")
	}
}

func TestConversationKeySkipsSystemAndEmpty(t *testing.T) {
	if got := conversationKey("m", "", []peekMessage{{Role: "system", Content: []byte(`"sys"`)}}); got != "" {
		t.Fatalf("system-only should yield empty key, got %q", got)
	}
	msgs := []peekMessage{
		{Role: "system", Content: []byte(`"sys"`)},
		{Role: "user", Content: []byte(`"  "`)},
		{Role: "tool", Content: []byte(`"result"`)},
	}
	if got := conversationKey("m", "", msgs); got == "" {
		t.Fatal("first non-empty non-system message should yield a key")
	}
}
