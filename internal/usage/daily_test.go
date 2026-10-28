package usage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDailyIndexCountsTodayAndRollsOver(t *testing.T) {
	r := New("")
	day1 := time.Date(2026, 10, 28, 10, 0, 0, 0, time.Local)
	r.Add(day1, "cn", "u1", "m1", Delta{TotalTokens: 100, HasTotal: true, Credit: 1.5, HasCredit: true}, true)
	r.Add(day1.Add(time.Hour), "cn", "u1", "m2", Delta{PromptTokens: 30, HasPromptTokens: true, CompletionTokens: 20, HasCompletion: true}, true)
	r.Add(day1, "cn", "u1", "m1", Delta{}, false) // 失败尝试不计 token

	d := r.Daily(day1, "u1", "m1")
	if d.Tokens != 150 || d.ModelTokens != 100 || d.Credits != 1.5 {
		t.Fatalf("day1 got %+v, want tokens=150 model=100 credits=1.5", d)
	}
	if m := r.DailyModels(day1, "u1"); m["m2"] != 50 {
		t.Fatalf("DailyModels got %v, want m2=50", m)
	}
	// 次日零点后清零。
	if d := r.Daily(day1.Add(24*time.Hour), "u1", "m1"); d.Tokens != 0 || d.Credits != 0 {
		t.Fatalf("次日 got %+v, want 0", d)
	}
}

func TestDailyIndexRebuiltOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	now := time.Now()
	r := New(path)
	r.Add(now, "cn", "u1", "m1", Delta{TotalTokens: 400, HasTotal: true, Credit: 3, HasCredit: true}, true)
	r.Add(now.Add(-48*time.Hour), "cn", "u1", "m1", Delta{TotalTokens: 999, HasTotal: true}, true) // 前天，不计入
	r.Save()

	r2 := New(path)
	d := r2.Daily(now, "u1", "m1")
	if d.Tokens != 400 || d.ModelTokens != 400 || d.Credits != 3 {
		t.Fatalf("重启后 got %+v, want tokens=400 credits=3（护栏计数不能因重启清零）", d)
	}
}
