// share.go 账号请求份额上限：防止单个账号吃掉整池流量。
//
// 背景（2026-11-02 本机实测）：28 个可用号的池子里，937 次请求有 512 次
// （55%）落在同一个号上，单小时 199 次请求只用了 1 个号。上游看到的是一个
// 「一小时 199 次、同一模型、同一 UA」的重度异常账号，而不是 28 个正常用户。
//
// 份额上限按「公平份额的倍数」表达：limit = cap × 窗口总请求 / 池内账号数。
// 默认 cap=2.0，即任何账号最多承担约两倍公平份额。只在**新会话选号**时生效，
// 已绑定的粘性会话永远不因份额被换号——换号会造成更严重的「一个会话横跨多号」。
package pool

import (
	"sort"
	"time"
)

// defaultAccountShareCap 单号份额上限默认倍数（2× 公平份额）。
const defaultAccountShareCap = 2.0

// shareWindowMinAccounts 池子太小时份额限制没有意义（2 个号各 50% 就是公平份额，
// cap=2 反而会把正常轮换全部拦住）。至少 4 个号才启用。
const shareWindowMinAccounts = 4

// shareWindowMinSamples 每个账号平均至少跑过 shareWindowMinSamples 次才开始限份额。
// 冷启动/低流量时限制会误伤：3 个请求不可能分给 28 个号。
const shareWindowMinSamples = 2

// SetAccountShareCap 注入单号份额上限倍数；<=0 关闭。
func (p *Pool) SetAccountShareCap(v float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v < 0 {
		v = 0
	}
	p.accountShareCap = v
}

// SetDailyRequestLimit 注入账号级每日请求数上限；<=0 关闭。
func (p *Pool) SetDailyRequestLimit(v int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v < 0 {
		v = 0
	}
	p.dailyRequestLimit = v
}

// rollShareWindowsLocked 在窗口滚动时清零全池计数。调用方必须已持 p.mu。
func (p *Pool) rollShareWindowsLocked(now time.Time) {
	hourStart := now.Truncate(time.Hour)
	if p.hourStart.IsZero() || !p.hourStart.Equal(hourStart) {
		p.hourStart = hourStart
		p.hourTotal = 0
		p.hourRealmTotals = map[string]int64{}
		for _, e := range p.byUID {
			e.hourReq = 0
		}
	}
	day := now.Format("2006-01-02")
	if p.dayStart == "" || p.dayStart != day {
		p.dayStart = day
		p.dayTotal = 0
		p.dayRealmTotals = map[string]int64{}
		for _, e := range p.byUID {
			e.dayReq = 0
		}
	}
}

// shareBlockedLocked 判断账号是否已吃满份额/日请求上限。仅用于新会话选号。
// 调用方必须已持 p.mu。
func (p *Pool) shareBlockedLocked(e *entry, now time.Time) bool {
	p.rollShareWindowsLocked(now)
	if p.dailyRequestLimit > 0 && e.dayReq >= p.dailyRequestLimit {
		return true
	}
	if p.accountShareCap <= 0 {
		return false
	}
	// 份额按 realm 分别计算：cn/global 是两条独立的账号池，用全池数量做分母会
	// 让小池（例如只有 2 个 cn 号）在低流量时被误判为「吃满份额」。
	n := int64(0)
	for _, other := range p.byUID {
		if other.a.Realm() == e.a.Realm() {
			n++
		}
	}
	realm := e.a.Realm()
	hourTotal := p.hourRealmTotals[realm]
	dayTotal := p.dayRealmTotals[realm]
	if n < shareWindowMinAccounts || hourTotal < n*shareWindowMinSamples {
		return false
	}
	// 再选一次这个号是否会让它超过 cap 倍公平份额。
	hourLimit := p.accountShareCap * float64(hourTotal) / float64(n)
	if float64(e.hourReq+1) > hourLimit {
		return true
	}
	if dayTotal < n*shareWindowMinSamples {
		return false
	}
	dayLimit := p.accountShareCap * float64(dayTotal) / float64(n)
	return float64(e.dayReq+1) > dayLimit
}

// notePickedLocked 记账一次选中（成功进入请求即计入，失败也计入——
// 份额约束的是「上游看到这个号被打了几次」，不是「成功了几次」）。
func (p *Pool) notePickedLocked(e *entry, now time.Time) {
	p.rollShareWindowsLocked(now)
	e.hourReq++
	p.hourTotal++
	p.hourRealmTotals[e.a.Realm()]++
	e.dayReq++
	p.dayTotal++
	p.dayRealmTotals[e.a.Realm()]++
}

// ShareStat 单账号请求份额快照（面板风控视图）。
type ShareStat struct {
	UID           string    `json:"uid"`
	Nickname      string    `json:"nickname"`
	HourReq       int64     `json:"hour_requests"`
	DayReq        int64     `json:"day_requests"`
	HourShare     float64   `json:"hour_share"`
	DayShare      float64   `json:"day_share"`
	RiskHold      bool      `json:"risk_hold"`
	RiskUntil     time.Time `json:"risk_until,omitempty"`
	RiskProbeDue  bool      `json:"risk_probe_due,omitempty"`
	Disabled      bool      `json:"disabled"`
	Cooling       bool      `json:"cooling"`
	InFlight      int       `json:"in_flight"`
	InFlightLimit int       `json:"in_flight_limit"`
}

// ShareSnapshot 返回当前窗口的全池请求份额快照，按小时请求数降序。
func (p *Pool) ShareSnapshot() (hourTotal, dayTotal int64, stats []ShareStat) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.rollShareWindowsLocked(now)
	stats = make([]ShareStat, 0, len(p.byUID))
	for uid, e := range p.byUID {
		st := ShareStat{
			UID:           uid,
			Nickname:      e.a.Nickname,
			HourReq:       e.hourReq,
			DayReq:        e.dayReq,
			Disabled:      e.disabled,
			Cooling:       now.Before(e.expiry(now)),
			InFlight:      int(e.inFlight.Load()),
			InFlightLimit: p.inFlightLimit(e),
		}
		if p.hourTotal > 0 {
			st.HourShare = float64(e.hourReq) / float64(p.hourTotal)
		}
		if p.dayTotal > 0 {
			st.DayShare = float64(e.dayReq) / float64(p.dayTotal)
		}
		if e.riskHold {
			st.RiskHold = true
			st.RiskUntil = e.riskUntil
			st.RiskProbeDue = !now.Before(e.riskUntil)
		}
		stats = append(stats, st)
	}
	sortShareStats(stats)
	return p.hourTotal, p.dayTotal, stats
}

// sortShareStats 按小时请求数降序、日请求数降序、UID 升序稳定排序。
func sortShareStats(stats []ShareStat) {
	sort.SliceStable(stats, func(i, j int) bool {
		if stats[i].HourReq != stats[j].HourReq {
			return stats[i].HourReq > stats[j].HourReq
		}
		if stats[i].DayReq != stats[j].DayReq {
			return stats[i].DayReq > stats[j].DayReq
		}
		return stats[i].UID < stats[j].UID
	})
}
