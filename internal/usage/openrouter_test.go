package usage

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// restorePricing 测试会替换全局生效价格表，结束后恢复内置快照。
func restorePricing(t *testing.T) {
	t.Helper()
	old := current.Load()
	t.Cleanup(func() { current.Store(old) })
}

func sampleOR() map[string]map[string]string {
	return map[string]map[string]string{
		"z-ai/glm-5.2":              {"prompt": "0.0000006", "completion": "0.0000022", "input_cache_read": "0.00000011"},
		"moonshotai/kimi-k3":        {"prompt": "0.000000192", "completion": "0.00002"},
		"moonshotai/kimi-k3:batch":  {"prompt": "0.0000001", "completion": "0.00001"},
		"deepseek/deepseek-r1-0528": {"prompt": "0.0000005", "completion": "0.00000215"},
		"a/dup-model":               {"prompt": "0.000001", "completion": "0.000001"},
		"b/dup-model":               {"prompt": "0.000002", "completion": "0.000002"},
		"tencent/hy3":               {"prompt": "0.0000002", "completion": "0.0000008"},
	}
}

func TestBuildPriceTableMatching(t *testing.T) {
	ids := []string{"glm-5.2", "kimi-k3-1", "deepseek-r1-0528-lkeap", "dup-model", "hy3-x", "my-alias", "nothing"}
	tb, unmatched := BuildPriceTable(sampleOR(), ids, map[string]string{"my-alias": "z-ai/glm-5.2"})

	glm := tb.Models["glm-5.2"]
	if glm.Via != "exact" || glm.ORID != "z-ai/glm-5.2" || glm.Input != 0.6 || glm.Output != 2.2 || glm.CacheHit == nil || *glm.CacheHit != 0.11 {
		t.Fatalf("glm-5.2 = %+v", glm)
	}
	if k := tb.Models["kimi-k3-1"]; k.Via != "override" || k.ORID != "moonshotai/kimi-k3" || k.CacheHit != nil {
		t.Fatalf("kimi-k3-1 应走内置映射且不取 :batch 变体: %+v", k)
	}
	if d := tb.Models["deepseek-r1-0528-lkeap"]; d.Via != "variant" || d.ORID != "deepseek/deepseek-r1-0528" {
		t.Fatalf("-lkeap 应剥后缀继承: %+v", d)
	}
	if h := tb.Models["hy3-x"]; h.Via != "override" {
		t.Fatalf("hy3-x 走内置映射: %+v", h)
	}
	if a := tb.Models["my-alias"]; a.Via != "override" || a.ORID != "z-ai/glm-5.2" {
		t.Fatalf("手填映射应生效: %+v", a)
	}
	sort.Strings(unmatched)
	if len(unmatched) != 2 || unmatched[0] != "dup-model" || unmatched[1] != "nothing" {
		t.Fatalf("同名多候选与无候选都应未匹配: %v", unmatched)
	}
}

func TestRefresherFallbackAndPersistence(t *testing.T) {
	restorePricing(t)
	dir := t.TempDir()
	fail := true
	fetch := func(context.Context) (map[string]map[string]string, error) {
		if fail {
			return nil, errors.New("network down")
		}
		return sampleOR(), nil
	}
	r := NewPriceRefresher(dir, func() []string { return []string{"glm-5.2"} }, fetch)
	if priceInfo().Dynamic {
		t.Fatal("没有动态表时应使用内置快照")
	}
	builtinGLM := pricesNow().table.Models["glm-5.2"]

	// 失败：沿用当前价格，记录错误。
	st := r.RefreshNow()
	if st.LastError == "" || pricesNow().table.Models["glm-5.2"] != builtinGLM {
		t.Fatalf("失败应沿用旧价: %+v", st)
	}

	// 成功：生效并落盘。
	fail = false
	st = r.RefreshNow()
	if st.LastError != "" || !priceInfo().Dynamic || pricesNow().table.Models["glm-5.2"].Input != 0.6 {
		t.Fatalf("刷新后应用新价: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "pricing.json")); err != nil {
		t.Fatalf("应落盘 pricing.json: %v", err)
	}

	// 重启：先用落盘的动态表，不必联网。
	current.Store(&pricing{table: builtinPrices()})
	NewPriceRefresher(dir, nil, func(context.Context) (map[string]map[string]string, error) { return nil, errors.New("offline") })
	if !priceInfo().Dynamic || pricesNow().table.Models["glm-5.2"].Input != 0.6 {
		t.Fatal("重启后应加载上次的动态价格表")
	}
}

func TestCalibrationMarkupAndRate(t *testing.T) {
	restorePricing(t)
	dir := t.TempDir()
	r := NewPriceRefresher(dir, func() []string { return []string{"glm-5.2"} }, func(context.Context) (map[string]map[string]string, error) { return sampleOR(), nil })
	r.RefreshNow()

	rec := New("")
	rec.Add(time.Now(), "cn", "u", "glm-5.2", Delta{PromptTokens: 1_000_000, HasPromptTokens: true}, true)
	base := rec.Snapshot(24, nil).Totals.CostUSD
	if math.Abs(base-0.6) > 1e-9 {
		t.Fatalf("base cost=%v want 0.6", base)
	}

	if _, err := r.SetSettings(PriceSettings{RefreshEnabled: true, RefreshHours: 24, UsdCny: 7.25, Markup: map[string]float64{"glm-5.2": 1.5, "bad": -1}}); err != nil {
		t.Fatal(err)
	}
	s := rec.Snapshot(24, nil)
	if math.Abs(s.Totals.CostUSD-0.9) > 1e-9 || math.Abs(s.Totals.CostCNY-0.9*7.25) > 1e-9 {
		t.Fatalf("校准后 usd=%v cny=%v", s.Totals.CostUSD, s.Totals.CostCNY)
	}
	if !s.Pricing.Calibrated || s.Pricing.UsdCny != 7.25 {
		t.Fatalf("pricing info=%+v", s.Pricing)
	}
	if _, ok := r.Settings().Markup["bad"]; ok {
		t.Fatal("非正系数应被丢弃")
	}
	// 设置持久化。
	r2 := NewPriceRefresher(dir, nil, func(context.Context) (map[string]map[string]string, error) { return nil, errors.New("offline") })
	if r2.Settings().UsdCny != 7.25 || r2.Settings().Markup["glm-5.2"] != 1.5 {
		t.Fatalf("设置未持久化: %+v", r2.Settings())
	}
}

func TestPriceSettingsNormalize(t *testing.T) {
	s := PriceSettings{RefreshHours: -3, UsdCny: -1, Markup: map[string]float64{" m ": 1, "x": 2}, Mapping: map[string]string{"a": " ", " b ": " or/b "}}.Normalize()
	if s.RefreshHours != 24 || s.UsdCny != 0 || len(s.Markup) != 1 || s.Markup["x"] != 2 || s.Mapping["b"] != "or/b" || len(s.Mapping) != 1 {
		t.Fatalf("normalize = %+v", s)
	}
	if (PriceSettings{RefreshHours: 9999}).Normalize().RefreshHours != 720 {
		t.Fatal("间隔上限 720h")
	}
}

// TestStopAbortsInFlightFetch 关停时进行中的拉取必须被中止，Stop 不能等网络。
func TestStopAbortsInFlightFetch(t *testing.T) {
	restorePricing(t)
	entered := make(chan struct{})
	r := NewPriceRefresher(t.TempDir(), nil, func(ctx context.Context) (map[string]map[string]string, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	r.startDelay = 0
	r.Start() // 无落盘表 → 立即到期，后台开始拉取
	<-entered
	done := make(chan struct{})
	go func() { r.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop 被进行中的拉取卡住")
	}
	if st := r.Status(); st.LastError != "" {
		t.Fatalf("关停导致的取消不应记成刷新失败: %q", st.LastError)
	}
}
