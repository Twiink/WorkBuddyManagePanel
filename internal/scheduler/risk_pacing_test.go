package scheduler

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestAccountGapDurationRange(t *testing.T) {
	gap := 30 * time.Second
	if got := accountGapDuration(gap, func(int64) int64 { return 0 }); got != 20*time.Second {
		t.Fatalf("lower bound=%v want 20s", got)
	}
	if got := accountGapDuration(gap, func(n int64) int64 { return n - 1 }); got < 59*time.Second || got >= 60*time.Second {
		t.Fatalf("upper bound=%v want [59s,60s)", got)
	}
	if got := accountGapDuration(3*time.Second, func(int64) int64 { return 0 }); got < 2*time.Second || got >= 3*time.Second {
		t.Fatalf("small gap=%v want [2s,3s)", got)
	}
}

func TestPacingAuditReflectsHotConfig(t *testing.T) {
	s := New(Config{JitterMinutes: 25, AccountSpacing: 30 * time.Second})
	jitter, spacing := s.PacingAudit()
	if jitter != 25 || spacing != 30*time.Second {
		t.Fatalf("audit=%d/%v", jitter, spacing)
	}
	s.SetTaskPacing(10, 45*time.Second)
	jitter, spacing = s.PacingAudit()
	if jitter != 10 || spacing != 45*time.Second {
		t.Fatalf("audit after update=%d/%v", jitter, spacing)
	}
}

func TestJitteredIsBoundedDeterministicAndTaskSeparated(t *testing.T) {
	s := New(Config{JitterMinutes: 25})
	base := time.Date(2026, 11, 2, 9, 0, 0, 0, time.Local)
	a1 := s.jittered(base, "checkin")
	a2 := s.jittered(base, "checkin")
	if !a1.Equal(a2) {
		t.Fatalf("same task/slot jitter drifted: %v != %v", a1, a2)
	}
	if delta := a1.Sub(base); delta < -25*time.Minute || delta > 25*time.Minute {
		t.Fatalf("jitter %v outside ±25m", delta)
	}
	travel := s.jittered(base, "travel")
	if travel.Equal(a1) {
		t.Fatalf("different task kinds should not share the same offset: %v", a1)
	}
}

func TestRiskHeldAccountSkippedByAutomaticTasks(t *testing.T) {
	fastActivity(t)
	stub := &reportStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "held", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "ok", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Quarantine("held", time.Hour, "11140")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, AccountSpacing: 0})
	s.RunActivityNow()
	if got := stub.calls.Load(); got != 1 {
		t.Fatalf("automatic activity calls=%d want only healthy account", got)
	}
}
