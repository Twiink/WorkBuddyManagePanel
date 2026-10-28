package usage

import (
	"math"
	"testing"
	"time"
)

// 估算价值：输入按缓存命中/未命中分开计价，输出按输出价；不在价格表的模型不计价并列出。
func TestSnapshotCostEstimate(t *testing.T) {
	r := New("")
	now := time.Now()
	// deepseek-v4.1-flash：未命中 0.03、命中 0.01、输出 0.5（USD / 1M）。
	r.Add(now, "cn", "u1", "cn:deepseek-v4.1-flash", Delta{
		PromptTokens: 1_000_000, HasPromptTokens: true,
		CompletionTokens: 1_000_000, HasCompletion: true,
		HasCacheTokens: true, CacheHitTokens: 800_000, CacheMissTokens: 200_000,
	}, true)
	r.Add(now, "cn", "u1", "no-such-model", Delta{PromptTokens: 10, HasPromptTokens: true}, true)

	s := r.Snapshot(24, nil)
	want := (200_000*0.03 + 800_000*0.01 + 1_000_000*0.5) / 1e6
	if math.Abs(s.Totals.CostUSD-want) > 1e-9 {
		t.Fatalf("cost_usd = %v, want %v", s.Totals.CostUSD, want)
	}
	if math.Abs(s.Totals.CostCNY-want*s.Pricing.UsdCny) > 1e-9 {
		t.Fatalf("cost_cny = %v, want %v", s.Totals.CostCNY, want*s.Pricing.UsdCny)
	}
	if s.Totals.UnpricedTokens != 10 || len(s.UnpricedModels) != 1 || s.UnpricedModels[0] != "no-such-model" {
		t.Fatalf("unpriced = %d %v", s.Totals.UnpricedTokens, s.UnpricedModels)
	}
	for _, row := range s.ByModel {
		if row.Key == "no-such-model" && row.CostUSD != 0 {
			t.Fatalf("未知模型不应计价: %+v", row)
		}
	}
}

// 没有公布命中价的模型，命中部分按未命中价计（宁高勿低）。
func TestCostCacheHitFallsBackToMissPrice(t *testing.T) {
	p, ok := prices.Models["kimi-k2-thinking"]
	if !ok || p.CacheHit != nil {
		t.Skip("价格表里 kimi-k2-thinking 已有命中价")
	}
	b := &bucket{Model: "kimi-k2-thinking", PT: 1_000_000, CH: 1_000_000}
	usd, ok := bucketCostUSD(b)
	if !ok || math.Abs(usd-p.Input) > 1e-9 {
		t.Fatalf("usd = %v, want %v", usd, p.Input)
	}
}
