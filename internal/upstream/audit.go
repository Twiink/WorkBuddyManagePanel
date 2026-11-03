// audit.go 出站协议头完整度自检：用真实 ChatHeaders 构造一条样例请求，
// 检查官方网关要求的头是否齐全。供面板「风控」视图直接展示，避免配置漂移
// （例如把 client_version 写空、或将来改头时漏掉某一项）无人发现。
package upstream

import (
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// requiredChatHeaders 官方 applyGatewayHeaders + applyTraceHeaders 必发头。
// X-Device-Token 不在此列：没有真实 Turing token 时伪造比缺失更危险。
var requiredChatHeaders = []string{
	"X-Conversation-ID",
	"X-Conversation-Request-ID",
	"X-Conversation-Message-ID",
	"X-Request-ID",
	"X-Root-Request-ID",
	"X-Trace-ID",
	"traceparent",
	"b3",
	"X-B3-TraceId",
	"X-B3-SpanId",
	"X-B3-ParentSpanId",
	"X-B3-Sampled",
	"X-Agent-Intent",
	"X-Agent-Purpose",
	"X-IDE-Type",
	"X-IDE-Name",
	"X-IDE-Version",
	"X-Private-Data",
	"X-Product",
	"X-Machine-ID",
	"X-Session-ID",
	"X-CodeBuddy-Request",
	"acp-connection-id",
}

// HeaderAudit 返回当前出站配置下缺失的必发头（空切片 = 完整）。
func (c *Client) HeaderAudit() []string {
	req, err := http.NewRequest(http.MethodPost, "https://www.codebuddy.cn/v2/chat/completions", nil)
	if err != nil {
		return append([]string(nil), requiredChatHeaders...)
	}
	a := &auth.Auth{UID: "audit", AccessToken: "audit", ExpiresAt: 4102444800}
	c.ChatHeaders(req, a, "", ChatMeta{
		ConversationID:        "audit-conversation",
		ConversationRequestID: "0123456789abcdef0123456789abcdef",
	})
	var missing []string
	for _, h := range requiredChatHeaders {
		if req.Header.Get(h) == "" {
			missing = append(missing, h)
		}
	}
	return missing
}
