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
	ResetAt   *time.Time `json:"reset_at,omitempty"`
}

// keyUsageResponse 兼容 sub2api 的 GET /v1/usage 响应。remaining 供 Magpie
// 余额卡片（Balance URL 路径 /v1/usage 的默认字段 $remaining）。
type keyUsageResponse struct {
	Remaining  int64            `json:"remaining"`
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
//   - limit = remaining = 池内剩余积分合计，used = 0。
//
// used 刻意不取「已花/总额」：Magpie 把用量 ≥90% 的成员排到额度未知的成员
// 之后，而一个包快花完、余量当天作废恰是最该优先用掉的情形。这里的窗口表达
// 「到 reset_at 前可用的剩余积分」，而非套餐消耗进度。
//
// 池内无剩余积分或无任何未来到期批次时不给 rate_limits：Magpie 视为额度未知，
// 不会凭空给出重置时间。
func (h *Handler) keyUsage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, keyUsageOf(h.cfg.Pool.List(), time.Now()))
}

func keyUsageOf(accounts []pool.Status, now time.Time) keyUsageResponse {
	var remaining, earliestRemaining int64
	var earliest time.Time
	for _, st := range accounts {
		if st.Disabled || st.Paused || st.Credits <= 0 {
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
	out := keyUsageResponse{Remaining: remaining}
	if remaining <= 0 || earliest.IsZero() {
		return out
	}
	resetAt := earliest
	out.RateLimits = []keyUsageWindow{{
		Window:    "7d",
		Limit:     remaining,
		Used:      0,
		Remaining: remaining,
		ResetAt:   &resetAt,
	}}
	out.EarliestExpiryRemaining = earliestRemaining
	return out
}
