// 账号级每日护栏：每日 Token / 每日积分 / 单模型 Token。
package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// fakeDaily 测试用当日计数表：uid → 计数，模型 token 另表。
type fakeDaily struct {
	tokens  map[string]int64
	credits map[string]float64
	models  map[string]map[string]int64
}

func (f *fakeDaily) fn(_ time.Time, uid, model string) DailyCount {
	c := DailyCount{Tokens: f.tokens[uid], Credits: f.credits[uid]}
	if model != "" {
		c.ModelTokens = f.models[uid][model]
	}
	return c
}

func dailyPool(t *testing.T, l DailyLimits, f *fakeDaily) *Pool {
	t.Helper()
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetCredits("a", 1000, 0)
	p.SetCredits("b", 1000, 0)
	p.SetDailyLimits(l)
	p.SetDailyUsage(f.fn)
	p.SetDailyModels(func(_ time.Time, uid string) map[string]int64 { return f.models[uid] })
	return p
}

func pickAll(p *Pool, model string, n int) map[string]int {
	got := map[string]int{}
	for i := 0; i < n; i++ {
		if a := p.PickExcludingForRealm(nil, model, ""); a != nil {
			got[a.UID]++
		} else {
			got[""]++
		}
	}
	return got
}

func TestDailyTokenLimitHoldsOutWholeAccount(t *testing.T) {
	f := &fakeDaily{tokens: map[string]int64{"a": 500}}
	p := dailyPool(t, DailyLimits{Tokens: 500}, f)
	for _, model := range []string{"m1", "m2", ""} {
		if got := pickAll(p, model, 20); got["a"] != 0 || got["b"] != 20 {
			t.Fatalf("model=%q got %v, want only b（a 当日 token 已达上限）", model, got)
		}
	}
	// 两号都用满：选号返回 nil（全冷却兜底同样被拦）。
	f.tokens["b"] = 900
	if got := pickAll(p, "m1", 5); got[""] != 5 {
		t.Fatalf("全部用满 got %v, want all nil", got)
	}
	if n := p.DailyBlockedCount("m1", ""); n != 2 {
		t.Fatalf("DailyBlockedCount=%d, want 2", n)
	}
}

func TestDailyCreditLimitOnlyBlocksPaidModels(t *testing.T) {
	f := &fakeDaily{credits: map[string]float64{"a": 80}}
	p := dailyPool(t, DailyLimits{Credits: 80}, f)
	p.NoteModelCost("a", "paid", 2, 1000)
	p.NoteModelCost("b", "paid", 2, 1000)
	p.NoteModelCost("a", "free", 0, 1000)
	if got := pickAll(p, "paid", 20); got["a"] != 0 {
		t.Fatalf("收费模型 got %v, want a 被拦", got)
	}
	// 免费模型：a 不受限。b 无观测（tier 1）被成本分层排在 tier 0 之后，所以只会选 a。
	if got := pickAll(p, "free", 5); got["a"] != 5 {
		t.Fatalf("免费模型 got %v, want a（积分护栏不拦免费模型）", got)
	}
	// 目录倍率兜底：无实测时按目录倍率判收费。
	p.SetModelRateOf(func(_, model string) string {
		if model == "listed" {
			return "1.5"
		}
		return ""
	})
	if got := pickAll(p, "listed", 10); got["a"] != 0 {
		t.Fatalf("目录收费模型 got %v, want a 被拦", got)
	}
	if got := pickAll(p, "unknown", 20); got["a"] == 0 {
		t.Fatalf("倍率未知模型 got %v, want a 仍可选（未知按免费）", got)
	}
}

func TestModelDailyTokenLimitOnlyBlocksThatModel(t *testing.T) {
	f := &fakeDaily{models: map[string]map[string]int64{"a": {"hot": 300}}}
	p := dailyPool(t, DailyLimits{ModelTokens: 300}, f)
	if got := pickAll(p, "hot", 20); got["a"] != 0 || got["b"] != 20 {
		t.Fatalf("hot got %v, want only b", got)
	}
	if got := pickAll(p, "cold", 20); got["a"] == 0 {
		t.Fatalf("cold got %v, want a 仍可选", got)
	}
	st, _ := p.Status("a")
	if st.Daily == nil || len(st.Daily.BlockedModels) != 1 || st.Daily.BlockedModels[0] != "hot" {
		t.Fatalf("status daily=%+v, want blocked_models=[hot]", st.Daily)
	}
}

func TestDailyGuardStickyUnbinds(t *testing.T) {
	f := &fakeDaily{tokens: map[string]int64{"a": 10}}
	p := dailyPool(t, DailyLimits{Tokens: 10}, f)
	if a := p.PickByUIDForModel("a", "m"); a != nil {
		t.Fatalf("粘性号已达每日上限，want nil, got %v", a.UID)
	}
	if a := p.PickByUIDForModel("b", "m"); a == nil {
		t.Fatal("b 未达上限，粘性应命中")
	}
}

func TestDailyGuardOffByDefault(t *testing.T) {
	f := &fakeDaily{tokens: map[string]int64{"a": 1 << 40}}
	p := dailyPool(t, DailyLimits{}, f)
	if got := pickAll(p, "m", 20); got["a"] == 0 {
		t.Fatalf("护栏全关 got %v, want a 可选", got)
	}
	if st, _ := p.Status("a"); st.Daily != nil {
		t.Fatalf("护栏全关时 status.daily 应为 nil, got %+v", st.Daily)
	}
	p.SetDailyLimits(DailyLimits{Tokens: -5})
	if l := p.DailyLimitsValue(); l.Tokens != 0 {
		t.Fatalf("负值应钳 0, got %d", l.Tokens)
	}
}
