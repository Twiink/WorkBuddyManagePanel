package session

import (
	"net/http"
	"testing"
)

// 入站会话头优先级：x-conversation-id > x-session-id > x-claude-code-session-id > body。
func TestExtractKeyWithHeadersPriority(t *testing.T) {
	body := []byte(`{"metadata":{"conversation_id":"body-conv"},"messages":[]}`)
	h := http.Header{}
	if got := ExtractKeyWithHeaders(body, h); got != "body-conv" {
		t.Fatalf("body fallback=%q want body-conv", got)
	}
	h.Set("X-Claude-Code-Session-Id", "cc")
	if got := ExtractKeyWithHeaders(body, h); got != "cc" {
		t.Fatalf("claude header=%q want cc", got)
	}
	h.Set("X-Session-Id", "sess")
	if got := ExtractKeyWithHeaders(body, h); got != "sess" {
		t.Fatalf("session header should beat claude header, got %q", got)
	}
	h.Set("X-Conversation-Id", "conv")
	if got := ExtractKeyWithHeaders(body, h); got != "conv" {
		t.Fatalf("conversation header should win, got %q", got)
	}
}

func TestExtractKeyWithHeadersAndExplicit(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		header       http.Header
		wantExplicit bool
	}{
		{"header", `{"messages":[{"role":"user","content":"hi"}]}`, http.Header{"X-Session-Id": {"s1"}}, true},
		{"body conversation", `{"conversation_id":"c1"}`, nil, true},
		{"prompt cache", `{"prompt_cache_key":"p1"}`, nil, true},
		{"derived", `{"messages":[{"role":"user","content":"hi"}]}`, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key, explicit := ExtractKeyWithHeadersAndExplicit([]byte(tc.body), tc.header)
			if key == "" {
				t.Fatal("key should not be empty")
			}
			if explicit != tc.wantExplicit {
				t.Fatalf("explicit=%v want %v", explicit, tc.wantExplicit)
			}
		})
	}
}

// Responses 形态的顶层 session_id 必须能被识别，否则每轮都会退化成内容派生键。
func TestExtractKeyRecognizesSessionID(t *testing.T) {
	body := []byte(`{"session_id":"resp-1","messages":[{"role":"user","content":"hi"}]}`)
	if got := ExtractKey(body); got != "resp-1" {
		t.Fatalf("session_id=%q want resp-1", got)
	}
}

// 出站 conversation id 同样优先取请求头。
func TestResolveConversationIDWithHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-Session-Id", "sess-1")
	if got := ResolveConversationIDWithHeaders([]byte(`{"conversation_id":"body"}`), h); got != "sess-1" {
		t.Fatalf("header conversation id=%q want sess-1", got)
	}
	if got := ResolveConversationIDWithHeaders([]byte(`{"session_id":"resp"}`), http.Header{}); got != "resp" {
		t.Fatalf("body session_id=%q want resp", got)
	}
}
