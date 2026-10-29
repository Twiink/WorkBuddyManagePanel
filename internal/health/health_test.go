package health

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedTracker(path string, now *time.Time) *Tracker {
	t := New(path)
	t.now = func() time.Time { return *now }
	return t
}

func TestSnapshotCountsWindowAndKinds(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 30, 0, 0, time.Local)
	tr := fixedTracker("", &now)
	tr.Success("a")
	tr.Success("a")
	tr.Failure("a", "soft_rate", 429, "m1", "rate limited")
	tr.Failure("b", KindTransport, 0, "m1", "dial tcp: timeout")

	s := tr.Snapshot([]string{"a", "b", "c"})
	if s.OK24h != 2 || s.Err24h != 2 || s.SuccessRate != 50 {
		t.Fatalf("summary got ok=%d err=%d rate=%v", s.OK24h, s.Err24h, s.SuccessRate)
	}
	if len(s.Accounts) != 3 || s.Accounts[2].SuccessRate != -1 {
		t.Fatalf("无记录账号应补空行且成功率 -1: %+v", s.Accounts)
	}
	a := s.Accounts[0]
	if a.SuccessRate != 66.7 || a.LastErr == nil || a.LastErr.Status != 429 || a.Kinds24h[0].Kind != "soft_rate" {
		t.Fatalf("account a got %+v", a)
	}
	if len(s.Hourly) != windowHours || s.Hourly[windowHours-1].OK != 2 {
		t.Fatalf("hourly 末位应为当前小时: %+v", s.Hourly[windowHours-1])
	}

	// 25 小时后：窗口内计数清零，最近错误保留。
	now = now.Add(25 * time.Hour)
	tr.Success("a")
	s = tr.Snapshot([]string{"a"})
	if s.Accounts[0].OK24h != 1 || s.Accounts[0].Err24h != 0 || len(s.Accounts[0].Kinds24h) != 0 {
		t.Fatalf("窗口外应清零: %+v", s.Accounts[0])
	}
	if s.Accounts[0].LastErr == nil {
		t.Fatal("最近错误不随窗口清除")
	}
}

func TestBalanceStatusAndClip(t *testing.T) {
	now := time.Now()
	tr := fixedTracker("", &now)
	tr.Balance("a", errors.New("<html>"+strings.Repeat("x ", 400)+"</html>"))
	v := tr.Snapshot([]string{"a"}).Accounts[0]
	if v.BalanceErr == nil || len([]rune(v.BalanceErr.Msg)) > maxMsgRunes+1 {
		t.Fatalf("余额错误应截断: %+v", v.BalanceErr)
	}
	tr.Balance("a", nil)
	v = tr.Snapshot([]string{"a"}).Accounts[0]
	if v.BalanceErr != nil || v.BalanceOK.IsZero() {
		t.Fatalf("查询成功后应清掉余额错误: %+v", v)
	}
}

func TestPersistAndForget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.json")
	now := time.Now()
	tr := fixedTracker(path, &now)
	tr.Failure("a", "server", 502, "m", "bad gateway")
	tr.Stop()

	tr2 := fixedTracker(path, &now)
	if v := tr2.Snapshot([]string{"a"}).Accounts[0]; v.Err24h != 1 || v.LastErr == nil {
		t.Fatalf("重启后应恢复: %+v", v)
	}
	tr2.Forget("a")
	if v := tr2.Snapshot(nil); len(v.Accounts) != 0 {
		t.Fatalf("Forget 后不应再出现: %+v", v.Accounts)
	}
}

func TestNilTrackerIsSafe(t *testing.T) {
	var tr *Tracker
	tr.Success("a")
	tr.Failure("a", "x", 0, "", "")
	tr.Balance("a", nil)
	tr.Forget("a")
	tr.Stop()
	if s := tr.Snapshot([]string{"a"}); s.SuccessRate != -1 {
		t.Fatalf("nil tracker snapshot got %+v", s)
	}
}
