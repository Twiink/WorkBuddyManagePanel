package pool

import (
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestAcquireForModelCheckPausedAccountAndDailyRequestLimit(t *testing.T) {
	p := New("")
	defer p.Close()
	a := &auth.Auth{UID: "a"}
	p.Add(a)
	p.Pause(a.UID)
	p.SetDailyRequestLimit(1)
	if reason := p.AcquireForModelCheck(a, "model"); reason != "" {
		t.Fatal(reason)
	}
	if s, _ := p.Status(a.UID); !s.Paused || s.InFlight != 1 {
		t.Fatalf("paused=%t, in-flight=%d", s.Paused, s.InFlight)
	}
	p.Release(a.UID)
	if p.PickByUIDForModel(a.UID, "model") != nil {
		t.Fatal("manual check resumed normal routing")
	}
	if reason := p.AcquireForModelCheck(a, "next"); !strings.Contains(reason, "每日请求上限") {
		t.Fatalf("second request: %q", reason)
	}
	if _, total, _ := p.ShareSnapshot(); total != 1 {
		t.Fatalf("day requests=%d, want 1", total)
	}
}

func TestAcquireForModelCheckRetainsOtherGuards(t *testing.T) {
	p := New("")
	defer p.Close()
	a := &auth.Auth{UID: "a"}
	p.Add(a)
	p.SetCreditFloor(10)
	p.SetModelRateOf(func(_, model string) string {
		if model == "paid" {
			return "1"
		}
		return "0"
	})
	p.CooldownSoftForModel("a", time.Hour, time.Now().Add(time.Hour), "limited", "6004")
	p.SetDailyLimits(DailyLimits{ModelTokens: 5})
	p.SetDailyUsage(func(_ time.Time, _, model string) DailyCount {
		if model == "spent" {
			return DailyCount{ModelTokens: 5}
		}
		return DailyCount{}
	})
	for _, model := range []string{"paid", "limited", "spent"} {
		if reason := p.AcquireForModelCheck(a, model); reason == "" {
			t.Errorf("accepted blocked model %q", model)
		}
	}
	p.Quarantine("a", time.Hour, "11140")
	if reason := p.AcquireForModelCheck(a, "free"); reason == "" {
		t.Fatal("accepted quarantined account")
	}
	p.ReleaseRiskHold("a")
	p.Add(&auth.Auth{UID: "a"})
	if reason := p.AcquireForModelCheck(a, "free"); reason == "" {
		t.Fatal("accepted stale account credential")
	}
	if _, total, _ := p.ShareSnapshot(); total != 0 {
		t.Fatalf("blocked checks consumed %d requests", total)
	}
}

func TestAcquireForModelCheckConcurrentReservations(t *testing.T) {
	p := New("")
	defer p.Close()
	a := &auth.Auth{UID: "a"}
	p.Add(a)
	p.SetMaxInFlight(1)
	const callers = 20
	start := make(chan struct{})
	acquired := make(chan bool, callers)
	for i := 0; i < callers; i++ {
		go func() {
			<-start
			acquired <- p.AcquireForModelCheck(a, "model") == ""
		}()
	}
	close(start)
	count := 0
	for i := 0; i < callers; i++ {
		if <-acquired {
			count++
		}
	}
	if _, total, _ := p.ShareSnapshot(); total != 1 || count != 1 {
		t.Fatalf("acquired=%d, counted=%d; want one of each", count, total)
	}
	if p.Acquire(a.UID) {
		t.Fatal("regular request acquired the diagnostic's occupied slot")
	}
	p.Release(a.UID)
	if !p.Acquire(a.UID) {
		t.Fatal("slot still occupied after diagnostic finished")
	}
	p.Release(a.UID)
}
