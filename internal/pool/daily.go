// 账号级每日护栏：每日 Token 上限 / 每日积分上限 / 单模型每日 Token 上限。
//
// 三道护栏都按本地日历日计数，次日零点自动解除，0 = 关闭（缺省即现状）：
//   - 每日 Token：账号当日总 token 达到上限后，整号退出选号；
//   - 每日积分：当日实扣积分达到上限后，只拦收费模型（判据同积分保底），
//     免费模型照常可用——护栏的目的就是「留下免费额度」；
//   - 单模型 Token：只拦用满额度的那一个模型，同号其他模型照常可用。
//
// 计数来自 usage.Recorder 的当日索引（DailyFunc 注入，pool 不依赖 usage 包）。
// 未注入时三道护栏全部不生效。
package pool

import (
	"sort"
	"time"
)

// DailyLimits 每日护栏阈值（0 = 关闭）。
type DailyLimits struct {
	Tokens      int64 `json:"daily_token_limit"`
	Credits     int64 `json:"daily_credit_limit"`
	ModelTokens int64 `json:"model_daily_token_limit"`
}

// Enabled 是否有任一护栏开启。
func (l DailyLimits) Enabled() bool { return l.Tokens > 0 || l.Credits > 0 || l.ModelTokens > 0 }

// DailyCount 账号当日计数（DailyFunc 的返回）。ModelTokens 为所询模型的当日 token。
type DailyCount struct {
	Tokens      int64
	Credits     float64
	ModelTokens int64
}

// DailyFunc 查询账号 uid 当日计数；model 为空时不需要填 ModelTokens。
// 只在持 p.mu 时被调用，不得反向调用 Pool 方法。
type DailyFunc func(now time.Time, uid, model string) DailyCount

// 每日护栏拦截原因（/status 与日志口径）。
const (
	DailyBlockTokens      = "daily_tokens"
	DailyBlockCredits     = "daily_credits"
	DailyBlockModelTokens = "model_daily_tokens"
)

// SetDailyLimits 注入每日护栏阈值（负值钳 0）。热生效。
func (p *Pool) SetDailyLimits(l DailyLimits) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dailyLimits = DailyLimits{
		Tokens:      max(0, l.Tokens),
		Credits:     max(0, l.Credits),
		ModelTokens: max(0, l.ModelTokens),
	}
}

// DailyLimitsValue 透出生效的每日护栏阈值（/status 用）。
func (p *Pool) DailyLimitsValue() DailyLimits {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.dailyLimits
}

// SetDailyUsage 注入当日计数查询（gateway 用 usage.Recorder.Daily 装配）。nil = 护栏不生效。
func (p *Pool) SetDailyUsage(fn DailyFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dailyOf = fn
}

// dailyBlockReasonLocked 返回该账号对 model 的每日护栏拦截原因；不拦返回 ""。
// 调用方持有 p.mu。model 为空时只判账号级 Token 护栏（无模型无法判收费与单模型额度）。
func (p *Pool) dailyBlockReasonLocked(e *entry, model, realm string, now time.Time) string {
	l := p.dailyLimits
	if p.dailyOf == nil || !l.Enabled() {
		return ""
	}
	c := p.dailyOf(now, e.a.UID, model)
	if l.Tokens > 0 && c.Tokens >= l.Tokens {
		return DailyBlockTokens
	}
	if model == "" {
		return ""
	}
	if l.ModelTokens > 0 && c.ModelTokens >= l.ModelTokens {
		return DailyBlockModelTokens
	}
	if l.Credits > 0 && c.Credits >= float64(l.Credits) {
		if realm == "" {
			realm = e.a.Realm()
		}
		if p.modelPaidLocked(e, model, realm, now) {
			return DailyBlockCredits
		}
	}
	return ""
}

// DailyStatus 账号当日护栏状态（面板展示）。
type DailyStatus struct {
	Tokens  int64   `json:"tokens"`
	Credits float64 `json:"credits"`
	// Limits 生效阈值（面板据此画「已用 / 上限」）。
	Limits DailyLimits `json:"limits"`
	// TokenBlocked 账号级 Token 护栏已触发（整号停到次日零点）。
	TokenBlocked bool `json:"token_blocked,omitempty"`
	// CreditBlocked 积分护栏已触发（仅收费模型被拦）。
	CreditBlocked bool `json:"credit_blocked,omitempty"`
	// BlockedModels 单模型 Token 护栏已触发的模型。
	BlockedModels []string `json:"blocked_models,omitempty"`
}

// dailyStatusLocked 组装 statusOf 用的当日护栏状态；护栏全关或未注入时返回 nil。
func (p *Pool) dailyStatusLocked(e *entry, now time.Time) *DailyStatus {
	l := p.dailyLimits
	if p.dailyOf == nil || !l.Enabled() {
		return nil
	}
	c := p.dailyOf(now, e.a.UID, "")
	st := &DailyStatus{Tokens: c.Tokens, Credits: c.Credits, Limits: l}
	st.TokenBlocked = l.Tokens > 0 && c.Tokens >= l.Tokens
	st.CreditBlocked = l.Credits > 0 && c.Credits >= float64(l.Credits)
	if l.ModelTokens > 0 && p.dailyModelsOf != nil {
		for m, t := range p.dailyModelsOf(now, e.a.UID) {
			if t >= l.ModelTokens {
				st.BlockedModels = append(st.BlockedModels, m)
			}
		}
		sort.Strings(st.BlockedModels)
	}
	return st
}

// SetDailyModels 注入当日各模型 token 查询（面板展示单模型护栏用）。
func (p *Pool) SetDailyModels(fn func(now time.Time, uid string) map[string]int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dailyModelsOf = fn
}

// DailyBlockedCount 统计 realm 内因每日护栏而对 model 不可选的账号数（池空报错用）。
func (p *Pool) DailyBlockedCount(model, realm string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	n := 0
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if e.disabled || e.paused {
			continue
		}
		if p.dailyBlockReasonLocked(e, model, realm, now) != "" {
			n++
		}
	}
	return n
}
