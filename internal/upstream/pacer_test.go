package upstream

import (
	"context"
	"testing"
	"time"
)

func TestPacerSpacesSameAccount(t *testing.T) {
	p := NewPacer(40*time.Millisecond, 0)
	ctx := context.Background()
	if err := p.Wait(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := p.Wait(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("second wait=%v want >=40ms", elapsed)
	}
}

func TestPacerDifferentAccountsDoNotBlock(t *testing.T) {
	p := NewPacer(200*time.Millisecond, 0)
	ctx := context.Background()
	if err := p.Wait(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := p.Wait(ctx, "u2"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("different account blocked for %v", elapsed)
	}
}

func TestPacerRespectsContextCancel(t *testing.T) {
	p := NewPacer(time.Second, 0)
	if err := p.Wait(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.Wait(ctx, "u1"); err == nil {
		t.Fatal("want context error")
	}
}

func TestPacerDisabled(t *testing.T) {
	p := NewPacer(0, 0)
	start := time.Now()
	for i := 0; i < 100; i++ {
		if err := p.Wait(context.Background(), "u1"); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("disabled pacer blocked %v", elapsed)
	}
}

func TestPacerSnapshotRecordsIntervals(t *testing.T) {
	p := NewPacer(10*time.Millisecond, 0)
	for i := 0; i < 4; i++ {
		if err := p.Wait(context.Background(), "u1"); err != nil {
			t.Fatal(err)
		}
	}
	s := p.Snapshot()
	if len(s) != 1 {
		t.Fatalf("snapshot=%+v want one account", s)
	}
	if s[0].UID != "u1" || s[0].Samples != 3 {
		t.Fatalf("snapshot=%+v want uid=u1 samples=3", s[0])
	}
	if s[0].P50 < 5*time.Millisecond || s[0].Max > time.Second {
		t.Fatalf("unexpected interval stats: %+v", s[0])
	}
}

func TestPacerSettingsReflectHotUpdate(t *testing.T) {
	p := NewPacer(time.Second, 500*time.Millisecond)
	min, jitter := p.Settings()
	if min != time.Second || jitter != 500*time.Millisecond {
		t.Fatalf("settings=%v/%v want 1s/500ms", min, jitter)
	}
	p.Set(750*time.Millisecond, 250*time.Millisecond)
	min, jitter = p.Settings()
	if min != 750*time.Millisecond || jitter != 250*time.Millisecond {
		t.Fatalf("settings after update=%v/%v want 750ms/250ms", min, jitter)
	}
}
