// risk.go 面板「风控」只读视图：账号请求份额、真实并发、请求间隔分布、
// 11140/11-128/429 计数、会话换绑与出站协议头完整度。
// 数据全部来自实时内存/近 24h 健康台账，不产生任何上游请求。
package panel

import (
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/health"
)

// riskAlert 一条风控告警。
type riskAlert struct {
	Level   string `json:"level"` // warn / crit
	Code    string `json:"code"`
	Message string `json:"message"`
	UID     string `json:"uid,omitempty"`
}

type riskErrorCounts struct {
	SoftRate24h          int64 `json:"soft_rate_24h"`
	AccountRestricted24h int64 `json:"account_restricted_24h"`
	ChannelRefused24h    int64 `json:"channel_refused_24h"`
}

// riskSnapshot /panel/api/risk 响应体。
type riskSnapshot struct {
	HourTotal      int64             `json:"hour_total"`
	DayTotal       int64             `json:"day_total"`
	Accounts       []accountRiskView `json:"accounts"`
	Alerts         []riskAlert       `json:"alerts"`
	SessionRebinds int64             `json:"session_rebinds"`
	MissingHeaders []string          `json:"missing_headers"`
	ErrorCounts    riskErrorCounts   `json:"error_counts"`
	JitterMinutes  int               `json:"schedule_jitter_minutes"`
	AccountSpacing int64             `json:"schedule_account_spacing_sec"`
	CheckedAt      time.Time         `json:"checked_at"`
}

type accountRiskView struct {
	UID          string    `json:"uid"`
	Nickname     string    `json:"nickname"`
	HourReq      int64     `json:"hour_requests"`
	DayReq       int64     `json:"day_requests"`
	HourShare    float64   `json:"hour_share"`
	DayShare     float64   `json:"day_share"`
	RiskHold     bool      `json:"risk_hold"`
	RiskUntil    time.Time `json:"risk_until,omitempty"`
	RiskProbeDue bool      `json:"risk_probe_due,omitempty"`
	Disabled     bool      `json:"disabled"`
	Cooling      bool      `json:"cooling"`

	InFlight      int `json:"in_flight"`
	InFlightLimit int `json:"in_flight_limit"`

	IntervalSamples int   `json:"interval_samples"`
	IntervalP50Ms   int64 `json:"interval_p50_ms,omitempty"`
	IntervalP90Ms   int64 `json:"interval_p90_ms,omitempty"`
	IntervalMinMs   int64 `json:"interval_min_ms,omitempty"`
	IntervalMaxMs   int64 `json:"interval_max_ms,omitempty"`

	SoftRate24h          int64 `json:"soft_rate_24h"`
	AccountRestricted24h int64 `json:"account_restricted_24h"`
	ChannelRefused24h    int64 `json:"channel_refused_24h"`
	ConsecutiveSoftRate  int64 `json:"consecutive_soft_rate,omitempty"`
}

type accountRiskPacing struct {
	Samples int
	P50     int64
	P90     int64
	Min     int64
	Max     int64
}

// risk 输出风控快照。鉴权与其它 /panel/api/* 一致。
func (p *Panel) risk(w http.ResponseWriter, r *http.Request) {
	out := riskSnapshot{CheckedAt: time.Now(), Accounts: []accountRiskView{}, Alerts: []riskAlert{}, MissingHeaders: []string{}}
	var uids []string
	if p.cfg.Pool != nil {
		hourTotal, dayTotal, stats := p.cfg.Pool.ShareSnapshot()
		out.HourTotal, out.DayTotal = hourTotal, dayTotal
		n := len(stats)
		for _, s := range stats {
			uids = append(uids, s.UID)
			out.Accounts = append(out.Accounts, accountRiskView{
				UID: s.UID, Nickname: s.Nickname,
				HourReq: s.HourReq, DayReq: s.DayReq,
				HourShare: s.HourShare, DayShare: s.DayShare,
				RiskHold: s.RiskHold, RiskUntil: s.RiskUntil, RiskProbeDue: s.RiskProbeDue,
				Disabled: s.Disabled, Cooling: s.Cooling,
				InFlight: s.InFlight, InFlightLimit: s.InFlightLimit,
			})
			if s.RiskHold {
				out.Alerts = append(out.Alerts, riskAlert{
					Level: "warn", Code: "risk_hold", UID: s.UID,
					Message: "账号处于 11140 风控隔离，只有成功的只读余额探活才会回池",
				})
			}
			// 单号小时份额 > 8%：28 号池里正常应 ~3.6%，>8% 已经开始向单号集中。
			if n >= 4 && hourTotal >= int64(n) && s.HourShare > 0.08 {
				out.Alerts = append(out.Alerts, riskAlert{
					Level: "warn", Code: "share_hot", UID: s.UID,
					Message: "单号小时请求份额超过 8%，流量正在向少数账号集中",
				})
			}
			if s.InFlightLimit > 0 && s.InFlight > s.InFlightLimit {
				out.Alerts = append(out.Alerts, riskAlert{
					Level: "crit", Code: "account_concurrency", UID: s.UID,
					Message: "单账号在途请求超过配置上限，pacer 或租约释放链路可能被绕过",
				})
			}
		}
	}

	if p.cfg.Upstream != nil {
		pacingMin, _ := p.cfg.Upstream.PacingSettings()
		pacing := make(map[string]accountRiskPacing)
		for _, s := range p.cfg.Upstream.PacingSnapshot() {
			pacing[s.UID] = accountRiskPacing{
				Samples: s.Samples,
				P50:     s.P50.Milliseconds(),
				P90:     s.P90.Milliseconds(),
				Min:     s.Min.Milliseconds(),
				Max:     s.Max.Milliseconds(),
			}
		}
		for i := range out.Accounts {
			if s, ok := pacing[out.Accounts[i].UID]; ok {
				out.Accounts[i].IntervalSamples = s.Samples
				out.Accounts[i].IntervalP50Ms = s.P50
				out.Accounts[i].IntervalP90Ms = s.P90
				out.Accounts[i].IntervalMinMs = s.Min
				out.Accounts[i].IntervalMaxMs = s.Max
				p90 := time.Duration(s.P90) * time.Millisecond
				if s.Samples >= 3 && pacingMin > 0 && p90 > 0 && p90 < pacingMin*3/4 {
					out.Alerts = append(out.Alerts, riskAlert{
						Level: "warn", Code: "interval_tight", UID: out.Accounts[i].UID,
						Message: "单号请求间隔 P90 显著低于配置值，pacer 可能被绕过",
					})
				}
			}
		}
		out.MissingHeaders = p.cfg.Upstream.HeaderAudit()
		if len(out.MissingHeaders) > 0 {
			out.Alerts = append(out.Alerts, riskAlert{
				Level: "crit", Code: "header_incomplete",
				Message: "出站协议头不完整，上游通道校验会更容易判定为非官方客户端",
			})
		}
	}

	if p.cfg.Health != nil {
		hs := p.cfg.Health.Snapshot(uids)
		byUID := make(map[string]health.AccountView, len(hs.Accounts))
		for _, a := range hs.Accounts {
			byUID[a.UID] = a
			for _, k := range a.Kinds24h {
				switch k.Kind {
				case "soft_rate":
					out.ErrorCounts.SoftRate24h += k.Count
				case "account_restricted":
					out.ErrorCounts.AccountRestricted24h += k.Count
				case "channel_refused":
					out.ErrorCounts.ChannelRefused24h += k.Count
				}
			}
		}
		for i := range out.Accounts {
			if h, ok := byUID[out.Accounts[i].UID]; ok {
				for _, k := range h.Kinds24h {
					switch k.Kind {
					case "soft_rate":
						out.Accounts[i].SoftRate24h = k.Count
					case "account_restricted":
						out.Accounts[i].AccountRestricted24h = k.Count
					case "channel_refused":
						out.Accounts[i].ChannelRefused24h = k.Count
					}
				}
				out.Accounts[i].ConsecutiveSoftRate = h.SoftRateStreak
				if h.SoftRateStreak >= 2 {
					out.Alerts = append(out.Alerts, riskAlert{
						Level: "crit", Code: "consecutive_429", UID: h.UID,
						Message: "账号连续出现至少 2 次软限流，需暂停放量并检查请求节奏",
					})
				}
			}
		}
	}

	if p.cfg.Scheduler != nil {
		jitter, spacing := p.cfg.Scheduler.PacingAudit()
		out.JitterMinutes = jitter
		out.AccountSpacing = int64(spacing / time.Second)
		if jitter <= 0 || spacing < 20*time.Second {
			out.Alerts = append(out.Alerts, riskAlert{
				Level: "crit", Code: "schedule_burst",
				Message: "任务抖动/账号间隔低于防封基线，存在整点齐发风险",
			})
		}
	}

	if p.cfg.StickyRebinds != nil {
		out.SessionRebinds = p.cfg.StickyRebinds()
		if out.SessionRebinds > 0 {
			out.Alerts = append(out.Alerts, riskAlert{
				Level: "crit", Code: "session_rebind",
				Message: "同一会话换绑过账号：同一个 conversation_id 从多个账号发出",
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}
