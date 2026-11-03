package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/health"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestRiskEndpointReportsShareQuarantineAndErrors(t *testing.T) {
	pl := pool.New("")
	defer pl.Close()
	for _, uid := range []string{"hot", "b", "c", "d"} {
		pl.Add(&auth.Auth{UID: uid, AccessToken: "at", ExpiresAt: 9999999999})
		pl.SetCredits(uid, 1000, 0)
	}
	for i := 0; i < 12; i++ {
		// 粘性命中路径不做份额拦截，用它将可观测窗口稳定压到 hot 上；这里验证的是
		// 风控视图能识别既有集中度，而不是让普通 Pick 的公平轮换去碰随机结果。
		if a := pl.PickByUIDForModel("hot", ""); a == nil || a.UID != "hot" {
			t.Fatalf("sticky pick=%v want hot", a)
		}
	}
	pl.Quarantine("b", time.Hour, "11140")

	ht := health.New("")
	ht.Failure("hot", "soft_rate", 429, "glm-5.2", "rate")
	ht.Failure("hot", "soft_rate", 429, "glm-5.2", "rate")
	ht.Failure("c", "account_restricted", 403, "glm-5.2", "11140")
	ht.Failure("d", "channel_refused", 400, "glm-5.2", "11-128")

	up := upstream.New()
	sch := scheduler.New(scheduler.Config{JitterMinutes: 25, AccountSpacing: 30 * time.Second})
	p := New(Config{
		DesktopMode: true,
		Pool:        pl,
		Health:      ht,
		Upstream:    up,
		Scheduler:   sch,
		StickyRebinds: func() int64 {
			return 0
		},
	})

	rec := httptest.NewRecorder()
	p.DesktopHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/risk", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var got riskSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	if got.HourTotal != 12 {
		t.Fatalf("hour_total=%d want 12", got.HourTotal)
	}
	if got.ErrorCounts.SoftRate24h != 2 || got.ErrorCounts.AccountRestricted24h != 1 || got.ErrorCounts.ChannelRefused24h != 1 {
		t.Fatalf("error counts=%+v", got.ErrorCounts)
	}
	if len(got.MissingHeaders) != 0 {
		t.Fatalf("missing headers=%v", got.MissingHeaders)
	}
	codes := map[string]bool{}
	for _, a := range got.Alerts {
		codes[a.Code] = true
	}
	for _, want := range []string{"share_hot", "risk_hold", "consecutive_429"} {
		if !codes[want] {
			t.Errorf("missing alert %q in %+v", want, got.Alerts)
		}
	}
}

func TestRiskNavigationAndLoaderPresent(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`data-view="risk"`, `id="view-risk"`, `id="rkBody"`, `id="rk429"`,
		`name="jitter_minutes"`, `name="account_spacing_seconds"`,
		`name="account_share_cap"`, `name="daily_request_limit"`,
		`name="pacing_min_interval"`, `name="risk_quarantine"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q", want)
		}
	}
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/app.js", nil))
	js := rec.Body.String()
	for _, want := range []string{
		"loadRisk", "renderRisk", "interval_p90_ms", "soft_rate_24h",
		"pacing_min_interval", "risk_quarantine", "account_share_cap",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
}
