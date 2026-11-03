// pacer.go 账号级出站节流。
//
// 反代最容易暴露的不是单个请求的头部，而是节奏：28 个账号全部由同一进程、
// 同一模型、几乎等间隔地打上游，与真人使用完全不同。Pacer 给每个账号维护
// 一个下一次可用时刻，最小间隔之上再叠加随机抖动，把机械节奏打散。
package upstream

import (
	"context"
	"math/rand/v2"
	"sort"
	"sync"
	"time"
)

const pacerSampleCap = 64

type pacerAccountStat struct {
	last      time.Time
	intervals []time.Duration
	next      int
}

// PacingStat 单账号真实请求起始间隔的滚动统计（最多 64 个样本）。
type PacingStat struct {
	UID       string
	Samples   int
	P50       time.Duration
	P90       time.Duration
	Min       time.Duration
	Max       time.Duration
	LastStart time.Time
}

// Pacer 账号级出站节流器（并发安全）。min/jitter <= 0 时 Wait 直接放行。
type Pacer struct {
	mu     sync.Mutex
	next   map[string]time.Time
	stats  map[string]*pacerAccountStat
	min    time.Duration
	jitter time.Duration
}

// NewPacer 构造节流器。
func NewPacer(min, jitter time.Duration) *Pacer {
	return &Pacer{
		next:   map[string]time.Time{},
		stats:  map[string]*pacerAccountStat{},
		min:    min,
		jitter: jitter,
	}
}

// Set 热更新节流参数（面板改配置后生效）。
func (p *Pacer) Set(min, jitter time.Duration) {
	if p == nil {
		return
	}
	if min < 0 {
		min = 0
	}
	if jitter < 0 {
		jitter = 0
	}
	p.mu.Lock()
	p.min, p.jitter = min, jitter
	p.mu.Unlock()
}

// Settings 返回当前生效的最小间隔与抖动上限（热更新后面板据此按配置校验）。
func (p *Pacer) Settings() (min, jitter time.Duration) {
	if p == nil {
		return 0, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.min, p.jitter
}

// Wait 阻塞到该账号允许发起下一次请求；ctx 取消时立即返回。
// uid 为空时不做节流（匿名/无账号上下文）。
func (p *Pacer) Wait(ctx context.Context, uid string) error {
	if p == nil || uid == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.Lock()
	min, jitter := p.min, p.jitter
	if min <= 0 && jitter <= 0 {
		p.mu.Unlock()
		p.recordStart(uid)
		return nil
	}
	now := time.Now()
	slot := now
	if t, ok := p.next[uid]; ok && t.After(now) {
		slot = t
	}
	// 抖动只影响「这次请求实际等待多久」，并顺延到下一次槽位，避免每次固定间隔。
	var extra time.Duration
	if jitter > 0 {
		extra = rand.N(jitter)
	}
	delay := slot.Sub(now) + extra
	step := min + extra
	if step <= 0 {
		step = time.Millisecond
	}
	p.next[uid] = slot.Add(step)
	p.mu.Unlock()
	if delay <= 0 {
		p.recordStart(uid)
		return nil
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		p.recordStart(uid)
		return nil
	}
}

func (p *Pacer) recordStart(uid string) {
	if p == nil || uid == "" {
		return
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stats == nil {
		p.stats = map[string]*pacerAccountStat{}
	}
	st := p.stats[uid]
	if st == nil {
		st = &pacerAccountStat{}
		p.stats[uid] = st
	}
	if !st.last.IsZero() {
		if gap := now.Sub(st.last); gap >= 0 {
			if len(st.intervals) < pacerSampleCap {
				st.intervals = append(st.intervals, gap)
			} else {
				st.intervals[st.next] = gap
				st.next = (st.next + 1) % pacerSampleCap
			}
		}
	}
	st.last = now
}

// Snapshot 返回按 UID 排序的间隔统计。P50/P90 按实际请求起始时间计算，
// 不是配置目标值，因而能暴露并发抢跑、pacer 被绕过或请求突发。
func (p *Pacer) Snapshot() []PacingStat {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PacingStat, 0, len(p.stats))
	for uid, st := range p.stats {
		if st == nil || len(st.intervals) == 0 {
			continue
		}
		vals := append([]time.Duration(nil), st.intervals...)
		sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
		out = append(out, PacingStat{
			UID:       uid,
			Samples:   len(vals),
			P50:       vals[(len(vals)-1)*50/100],
			P90:       vals[(len(vals)-1)*90/100],
			Min:       vals[0],
			Max:       vals[len(vals)-1],
			LastStart: st.last,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}
