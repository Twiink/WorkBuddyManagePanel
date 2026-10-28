package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestKeyUsageOfEarliestBatch 7d 窗口的 reset_at 取参与选号账号中最早一批的到期时刻，
// 同一时刻的多账号批次合计；disabled/paused 与已过期批次不计入。
func TestKeyUsageOfEarliestBatch(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	soon := now.Add(30 * time.Hour)
	later := now.Add(10 * 24 * time.Hour)
	got := keyUsageOf([]pool.Status{
		{UID: "a", Credits: 500, CreditsEarliestExpiry: later, CreditsEarliestRemaining: 500},
		{UID: "b", Credits: 300, CreditsEarliestExpiry: soon, CreditsEarliestRemaining: 120},
		{UID: "c", Credits: 200, CreditsEarliestExpiry: soon, CreditsEarliestRemaining: 80},
		{UID: "d", Credits: 100}, // 无到期批次：计入剩余，不影响 reset_at
		{UID: "dis", Credits: 900, Disabled: true, CreditsEarliestExpiry: now.Add(time.Hour), CreditsEarliestRemaining: 900},
		{UID: "pz", Credits: 900, Paused: true, CreditsEarliestExpiry: now.Add(time.Hour), CreditsEarliestRemaining: 900},
		{UID: "old", Credits: 50, CreditsEarliestExpiry: now.Add(-time.Hour), CreditsEarliestRemaining: 50},
	}, now)

	if got.Remaining != 1150 {
		t.Fatalf("remaining=%d, want 1150", got.Remaining)
	}
	if len(got.RateLimits) != 1 {
		t.Fatalf("rate_limits=%v, want one 7d window", got.RateLimits)
	}
	w := got.RateLimits[0]
	if w.Window != "7d" || w.Limit != 1150 || w.Used != 0 || w.Remaining != 1150 {
		t.Fatalf("window=%+v", w)
	}
	if w.ResetAt == nil || !w.ResetAt.Equal(soon) {
		t.Fatalf("reset_at=%v, want %v", w.ResetAt, soon)
	}
	if got.EarliestExpiryRemaining != 200 {
		t.Fatalf("earliest_expiry_remaining=%d, want 200", got.EarliestExpiryRemaining)
	}
}

// TestKeyUsageOfNoExpiry 无未来到期批次或无剩余积分时不给 rate_limits（Magpie 视为未知）。
func TestKeyUsageOfNoExpiry(t *testing.T) {
	now := time.Now()
	for name, accounts := range map[string][]pool.Status{
		"empty":     nil,
		"no-expiry": {{UID: "a", Credits: 100}},
		"no-credit": {{UID: "a", Credits: 0, CreditsEarliestExpiry: now.Add(time.Hour), CreditsEarliestRemaining: 10}},
		"disabled":  {{UID: "a", Credits: 100, Disabled: true, CreditsEarliestExpiry: now.Add(time.Hour), CreditsEarliestRemaining: 10}},
	} {
		got := keyUsageOf(accounts, now)
		if len(got.RateLimits) != 0 {
			t.Errorf("%s: rate_limits=%v, want none", name, got.RateLimits)
		}
	}
}

// TestKeyUsageEndpoint 端到端：鉴权与 sub2api 字段形态（Magpie readSub2APIKeyLimits 读取的键）。
func TestKeyUsageEndpoint(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	expireAt := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	p.SetCreditsSnapshot("u1", pool.CreditsSnapshot{
		Remain: 800,
		Total:  1000,
		ExpiryBuckets: []pool.ExpiryBucket{
			{Date: expireAt.Format("2006-01-02"), ExpireAt: expireAt, Remain: 300},
		},
	})
	h := NewHandler(Config{Pool: p, APIKey: "sk-test"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/usage", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: status=%d, want 401", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Remaining  float64 `json:"remaining"`
		RateLimits []struct {
			Window  string     `json:"window"`
			Limit   float64    `json:"limit"`
			Used    float64    `json:"used"`
			ResetAt *time.Time `json:"reset_at"`
		} `json:"rate_limits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body)
	}
	if body.Remaining != 800 || len(body.RateLimits) != 1 {
		t.Fatalf("body=%s", rec.Body)
	}
	w := body.RateLimits[0]
	if w.Window != "7d" || w.Limit != 800 || w.Used != 0 || w.ResetAt == nil || !w.ResetAt.Equal(expireAt) {
		t.Fatalf("window=%+v, want 7d limit=800 used=0 reset_at=%v", w, expireAt)
	}
}
