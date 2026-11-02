package server

import (
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// keyUsageWindow sub2api /v1/usage 的 rate_limits 单个窗口。Magpie 的
// readSub2APIKeyLimits 只认 window ∈ {5h,1d,7d} 且 limit>0 的条目，按
// used/limit 算用量百分比、按 reset_at 判断「谁最先作废」。
type keyUsageWindow struct {
	Window    string     `json:"window"`
	Limit     int64      `json:"limit"`
	Used      int64      `json:"used"`
	Remaining int64      `json:"remaining"`
	Unit      string     `json:"unit,omitempty"`
	ResetAt   *time.Time `json:"reset_at,omitempty"`
}

// keyUsageResponse 兼容 sub2api 的 GET /v1/usage 响应。remaining 供 Magpie
// 余额卡片（Balance URL 路径 /v1/usage 的默认字段 $remaining）。
type keyUsageResponse struct {
	Remaining  int64            `json:"remaining"`
	Total      int64            `json:"total,omitempty"`
	Used       int64            `json:"used,omitempty"`
	Unit       string           `json:"unit,omitempty"`
	RateLimits []keyUsageWindow `json:"rate_limits,omitempty"`
	// EarliestExpiryRemaining 最早到期批次（全池同一时刻）的剩余积分，仅展示。
	EarliestExpiryRemaining int64 `json:"earliest_expiry_remaining,omitempty"`
}

// keyUsage 把账号池的积分到期压力按 sub2api 形态透出，让 Magpie 智能路由
// （「最先作废的额度先用」）能看到本网关的快到期积分。
//
// 口径：只统计会参与选号的账号（排除 disabled / paused；冷却中的照常计入，
// 冷却会自行到期）。窗口固定为 7d：
//   - reset_at = 这些账号中最早一批积分的到期时刻；
//   - remaining = 池内剩余积分合计；
//   - total / limit = 池内总额度合计；
//   - used = 总额度 - 剩余积分。
//
// 池内无剩余积分或无任何未来到期批次时不给 rate_limits：Magpie 视为额度未知，
// 不会凭空给出重置时间。
//
// total 是「当前套餐总额度」，不是累计充值。任一参与选号账号缺少总额度时，
// 无法可靠计算池级使用率，因此只返回 remaining，不伪造 100% 的窗口。
func (h *Handler) keyUsage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, keyUsageOf(h.cfg.Pool.List(), time.Now()))
}

func keyUsageOf(accounts []pool.Status, now time.Time) keyUsageResponse {
	var remaining, total, earliestRemaining int64
	var earliest time.Time
	totalKnown := true
	for _, st := range accounts {
		if st.Disabled || st.Paused {
			continue
		}
		if st.CreditsTotal > 0 {
			total += st.CreditsTotal
		} else {
			totalKnown = false
		}
		if st.Credits <= 0 {
			continue
		}
		remaining += st.Credits
		at := st.CreditsEarliestExpiry
		if at.IsZero() || !at.After(now) || st.CreditsEarliestRemaining <= 0 {
			continue
		}
		switch {
		case earliest.IsZero() || at.Before(earliest):
			earliest, earliestRemaining = at, st.CreditsEarliestRemaining
		case at.Equal(earliest):
			earliestRemaining += st.CreditsEarliestRemaining
		}
	}
	used := int64(0)
	if totalKnown && total > 0 && total >= remaining {
		used = total - remaining
	} else if !totalKnown {
		// 部分账号缺少总额度时，partial total 不是池级真实总量；宁可省略，
		// 也不要把已知账号的合计当成全池额度展示。
		total = 0
	}
	out := keyUsageResponse{
		Remaining: remaining,
		Total:     total,
		Used:      used,
		Unit:      "credits",
	}
	if remaining <= 0 || earliest.IsZero() || !totalKnown || total < remaining {
		return out
	}
	resetAt := earliest
	out.RateLimits = []keyUsageWindow{{
		Window:    "7d",
		Limit:     total,
		Used:      used,
		Remaining: remaining,
		Unit:      "credits",
		ResetAt:   &resetAt,
	}}
	out.EarliestExpiryRemaining = earliestRemaining
	return out
}
