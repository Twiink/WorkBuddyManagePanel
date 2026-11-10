package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestRateLimitFailoverAndStickyRecovery(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, identity := range []string{"anonymous", "header", "body", "sticky", "cooled-sticky"} {
			t.Run(fmt.Sprintf("%s/stream=%v", identity, stream), func(t *testing.T) {
				p := testPoolWith(
					&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
					&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
				)
				p.SetCredits("bad", 2000, 0)
				p.SetCredits("good", 1000, 0)
				sess := session.New(session.Config{TTL: time.Hour, AvailableForModel: p.AvailableUIDsForModel})
				if identity == "sticky" || identity == "cooled-sticky" {
					sess.Bind("client-session", "bad")
				}
				reset := time.Now().Add(time.Hour)
				if identity == "cooled-sticky" {
					p.CooldownSoftForModel("bad", time.Minute, reset, "kimi-k3.1-pro", "6004 model rate limit")
				}
				type attempt struct{ account, conversation, turn, trace string }
				var attempts []attempt
				up := &upstream.Client{
					ChatBaseCN: "https://fake.example",
					HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
						a := attempt{r.Header.Get("Authorization"), r.Header.Get("X-Conversation-ID"), r.Header.Get("X-Conversation-Request-ID"), r.Header.Get("X-Trace-ID")}
						attempts = append(attempts, a)
						status, body, ct := http.StatusOK, sseOK, "text/event-stream"
						if a.account == "Bearer at-bad" {
							status, ct = http.StatusTooManyRequests, "application/json"
							body = fmt.Sprintf(`{"code":6004,"msg":"将在 %s UTC+8 重置"}`, reset.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05"))
						}
						return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {ct}}, Body: io.NopCloser(strings.NewReader(body))}, nil
					})},
				}
				h := NewHandler(Config{Pool: p, Upstream: up, Session: sess})
				for n := 0; n < 2; n++ {
					meta := ""
					if identity == "body" {
						meta = `,"metadata":{"conversation_id":"client-session"}`
					}
					body := fmt.Sprintf(`{"model":"kimi-k3.1-pro","stream":%v,"messages":[{"role":"user","content":"hello"}]%s}`, stream, meta)
					req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
					if identity != "anonymous" && identity != "body" {
						req.Header.Set("X-Session-ID", "client-session")
					}
					req.Header.Set("X-Conversation-Request-ID", "client-turn")
					req.Header.Set("X-Trace-ID", "0123456789abcdef0123456789abcdef")
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					if rec.Code != http.StatusOK {
						t.Fatalf("request %d: status=%d body=%s", n, rec.Code, rec.Body.String())
					}
				}
				want := 3
				if identity == "cooled-sticky" {
					want = 2
				}
				if len(attempts) != want {
					t.Fatalf("attempts=%+v want %d", attempts, want)
				}
				firstGood, nextGood := attempts[len(attempts)-2], attempts[len(attempts)-1]
				if firstGood.account != "Bearer at-good" || nextGood.account != firstGood.account {
					t.Fatalf("successful attempts=%+v/%+v", firstGood, nextGood)
				}
				if identity != "anonymous" && (firstGood.conversation != nextGood.conversation || firstGood.turn != nextGood.turn || firstGood.trace != nextGood.trace) {
					t.Fatalf("new account identifiers must remain stable: %+v/%+v", firstGood, nextGood)
				}
				if want == 3 && (attempts[0].conversation == firstGood.conversation || attempts[0].turn == firstGood.turn || attempts[0].trace == firstGood.trace) {
					t.Fatalf("account identifiers must be isolated: %+v", attempts)
				}
				if identity != "anonymous" {
					if uid, ok := sess.LookupBinding("client-session"); !ok || uid != "good" {
						t.Fatalf("binding=%q/%v want good", uid, ok)
					}
				}
				st, _ := p.Status("bad")
				if st.Cooling || len(st.RateLimitedModels) != 1 {
					t.Fatalf("6004 must only cool this model: %+v", st)
				}
			})
		}
	}
}

func TestRateLimitRetryBudgetDoesNotDeclarePoolQuota(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 429, `{"code":6004,"msg":"将在 2099-11-10 17:11:06 UTC+8 重置"}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"kimi-k3.1-pro","messages":[]}`)))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "account_rotation_exhausted") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "2099") || strings.Contains(rec.Body.String(), "6004") {
		t.Fatalf("single-account reset must not pause the provider: %s", rec.Body.String())
	}
}
