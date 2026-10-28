package usage

import (
	_ "embed"
	"encoding/json"
	"log"
)

// 估算价值：把每个桶的 token 按模型公布单价折成美元，再按快照汇率折成人民币。
// 它回答的是「同样的 token 按官方公布价要花多少钱」，不是 WorkBuddy 的实际结算
// （实际结算看积分）。单价表 prices.json 取自 workbuddy2api-hub 的 OpenRouter
// 价格快照（模型级公布价，USD / 1M token）。
//
// 计费口径：
//   - 输入按「缓存命中」与「未命中」分开计价：命中部分 = min(缓存命中, 输入)，
//     其余输入按未命中价；模型没公布命中价时按未命中价计（宁高勿低）。
//   - 输出只按 completion_tokens 计（推理 token 已含在其中，不重复计）。
//   - 按输入长度 / 时段分档的模型只取基础档：桶内是聚合量，拿不到单条请求的
//     输入长度和时间点，长上下文请求会被低估。
//   - 不在表内的模型不计价，单独列在 unpriced_models 里，不猜价格。

//go:embed prices.json
var pricesJSON []byte

type modelPrice struct {
	Input    float64  `json:"input"`
	Output   float64  `json:"output"`
	CacheHit *float64 `json:"cache_hit,omitempty"`
}

type priceTable struct {
	Source      string                `json:"source"`
	GeneratedAt string                `json:"generated_at"`
	UsdCny      float64               `json:"usd_cny"`
	Unit        float64               `json:"unit"`
	Models      map[string]modelPrice `json:"models"`
}

var prices = loadPrices()

func loadPrices() priceTable {
	var t priceTable
	if err := json.Unmarshal(pricesJSON, &t); err != nil {
		log.Printf("[usage] 价格表解析失败（估算价值不可用）: %v", err)
	}
	if t.Unit <= 0 {
		t.Unit = 1_000_000
	}
	if t.UsdCny <= 0 {
		t.UsdCny = 7.1
	}
	return t
}

// bucketCostUSD 按单价估算一个桶的美元价值；ok=false 表示模型不在价格表内。
func bucketCostUSD(b *bucket) (float64, bool) {
	p, ok := prices.Models[canonicalUsageModel(b.Model)]
	if !ok {
		return 0, false
	}
	hitRate := p.Input
	if p.CacheHit != nil {
		hitRate = *p.CacheHit
	}
	cached := b.CH
	if cached > b.PT {
		cached = b.PT
	}
	if cached < 0 {
		cached = 0
	}
	uncached := b.PT - cached
	usd := (float64(uncached)*p.Input + float64(cached)*hitRate + float64(b.CT)*p.Output) / prices.Unit
	return usd, true
}

// PriceInfo 面板展示价格口径用的元信息。
type PriceInfo struct {
	Source      string  `json:"source"`
	GeneratedAt string  `json:"generated_at"`
	UsdCny      float64 `json:"usd_cny"`
}
