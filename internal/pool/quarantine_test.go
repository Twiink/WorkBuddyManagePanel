package pool

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 11140 隔离到点后只允许探活，不能仅凭时间自动回池。
func TestQuarantineRequiresSuccessfulProbe(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1"})
	p.Quarantine("u1", 30*time.Millisecond, "11140")
	if a := p.Pick(); a != nil {
		t.Fatalf("quarantined account must not be picked, got %s", a.UID)
	}
	if until, ok := p.RiskHoldUntil("u1"); !ok || until.IsZero() {
		t.Fatalf("RiskHoldUntil=%v/%v want active", until, ok)
	}
	time.Sleep(45 * time.Millisecond)
	if !p.RiskProbeDue("u1", time.Now()) {
		t.Fatal("probe should be due after quarantine interval")
	}
	if a := p.Pick(); a != nil {
		t.Fatalf("quarantine must remain held until a successful probe, got %s", a.UID)
	}
	p.ReenableIfCredits("u1", 100, 100)
	if _, ok := p.RiskHoldUntil("u1"); ok {
		t.Fatal("successful probe should clear risk hold")
	}
	if a := p.Pick(); a == nil || a.UID != "u1" {
		t.Fatalf("probed account should return to pool, got %+v", a)
	}
}

func TestFailedProbeDefersQuarantine(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1"})
	p.Quarantine("u1", 20*time.Millisecond, "11140")
	time.Sleep(35 * time.Millisecond)
	if !p.RiskProbeDue("u1", time.Now()) {
		t.Fatal("probe should be due")
	}
	p.DeferRiskProbe("u1", time.Hour)
	if p.RiskProbeDue("u1", time.Now()) {
		t.Fatal("failed probe must defer the next attempt")
	}
	if _, ok := p.RiskHoldUntil("u1"); !ok {
		t.Fatal("failed probe must keep risk hold")
	}
}

// 人工解冻必须清除隔离标记。
func TestReviveClearsQuarantine(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1"})
	p.Quarantine("u1", time.Hour, "11140")
	if !p.Revive("u1") {
		t.Fatal("revive failed")
	}
	if _, ok := p.RiskHoldUntil("u1"); ok {
		t.Fatal("revive must clear risk hold")
	}
	if a := p.Pick(); a == nil {
		t.Fatal("revived account should be pickable")
	}
}

// 隔离状态跨重启持久化，防止重启绕过 24h 冷却。
func TestQuarantinePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	p1 := New(path)
	p1.Add(&auth.Auth{UID: "u1"})
	p1.Quarantine("u1", time.Hour, "11140")
	p1.Flush()
	p1.Close()

	p2 := New(path)
	defer p2.Close()
	p2.Add(&auth.Auth{UID: "u1"})
	until, ok := p2.RiskHoldUntil("u1")
	if !ok || until.IsZero() {
		t.Fatalf("risk hold not restored: until=%v ok=%v", until, ok)
	}
	if got := p2.Pick(); got != nil {
		t.Fatalf("restored quarantine must not be pickable, got %s", got.UID)
	}
}
