package upstream

import (
	"net/http"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 默认配置下官方网关要求的头必须齐全。
func TestHeaderAuditCompleteByDefault(t *testing.T) {
	c := New()
	if missing := c.HeaderAudit(); len(missing) != 0 {
		t.Fatalf("missing headers: %v", missing)
	}
}

// 官方 applyGatewayHeaders / applyTraceHeaders 的关键头逐项断言，
// 防止后续重构把某一项悄悄删掉。
func TestChatHeadersIncludeOfficialGatewayHeaders(t *testing.T) {
	c := New()
	req, err := http.NewRequest(http.MethodPost, "https://www.codebuddy.cn/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	c.ChatHeaders(req, a, "", ChatMeta{
		ConversationID:        "conv-1",
		ConversationRequestID: "0123456789abcdef0123456789abcdef",
	})
	cases := map[string]string{
		"X-Agent-Intent":    "craft",
		"X-Agent-Purpose":   "conversation",
		"X-Private-Data":    "true",
		"X-Conversation-ID": "conv-1",
		"acp-connection-id": "",
		"traceparent":       "",
		"b3":                "",
		"X-B3-ParentSpanId": "",
		"X-B3-Sampled":      "1",
		"X-IDE-Version":     "5.9.3",
		"X-Product":         "WorkBuddy",
	}
	for h, want := range cases {
		got := req.Header.Get(h)
		if got == "" {
			t.Errorf("header %s missing", h)
			continue
		}
		if want != "" && got != want {
			t.Errorf("header %s=%q want %q", h, got, want)
		}
	}
}
