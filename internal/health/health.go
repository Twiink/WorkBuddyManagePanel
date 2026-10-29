// Package health 账号级健康与错误可观测性：逐账号记录近 24 小时成功/失败、
// 按错误类型计数、最近一次错误，以及余额查询的成败状态。
//
// 与已有观测的分工：
//   - pool.Status 回答「此刻能不能用」（冷却/熔断/禁用）；
//   - usage 回答「用了多少」；
//   - reqlog 是逐请求流水（有界、脱敏）；
//   - 本包回答「这个号最近健不健康、错在哪、余额还查不查得到」——按账号聚合，
//     冷却到期后状态消失也不影响回看。
//
// 记录点全部是旁路（不阻塞请求链路）：内存累加 + 防抖落盘。
package health

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// windowHours 滚动窗口长度（小时桶个数）。
const windowHours = 24

// flushInterval 防抖落盘间隔。
const flushInterval = 30 * time.Second

// maxMsgRunes 错误原文保留长度（防止整页 HTML 进状态文件）。
const maxMsgRunes = 240

// hourLayout 小时桶键格式（本地时区）。
const hourLayout = "2006-01-02T15"

// KindTransport 传输层失败（连不上上游/超时），不属于 upstream.ErrKind。
const KindTransport = "transport"

// LastError 最近一次错误。
type LastError struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"`
	Status int       `json:"status,omitempty"`
	Model  string    `json:"model,omitempty"`
	Msg    string    `json:"msg,omitempty"`
}

// hourBucket 一小时的成功/失败计数。
type hourBucket struct {
	Hour string `json:"h"`
	OK   int64  `json:"ok"`
	Err  int64  `json:"err"`
}

// account 单账号的累计态（落盘结构）。
type account struct {
	Hours      []hourBucket                `json:"hours,omitempty"`
	KindsHour  map[string]map[string]int64 `json:"kinds_hour,omitempty"` // 小时 → 错误类型 → 次数（窗口内）
	LastErr    *LastError                  `json:"last_err,omitempty"`
	LastOK     time.Time                   `json:"last_ok,omitempty"`
	BalanceOK  time.Time                   `json:"balance_ok,omitempty"`
	BalanceErr *LastError                  `json:"balance_err,omitempty"`
}

type diskFile struct {
	Version  int                 `json:"version"`
	Saved    string              `json:"saved,omitempty"`
	Accounts map[string]*account `json:"accounts"`
}

// Tracker 并发安全的健康记录器。path 为空 = 纯内存。
type Tracker struct {
	mu    sync.Mutex
	path  string
	accts map[string]*account
	dirty bool
	now   func() time.Time

	started  bool
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// New 创建记录器并加载落盘数据（失败从零开始）。
func New(path string) *Tracker {
	t := &Tracker{path: path, accts: map[string]*account{}, now: time.Now,
		stop: make(chan struct{}), done: make(chan struct{})}
	if path != "" {
		if err := t.load(); err != nil {
			log.Printf("[health] 读取 %s 失败（从零开始）: %v", path, err)
		}
	}
	return t
}

// Start 启动后台防抖落盘。
func (t *Tracker) Start() {
	if t == nil {
		return
	}
	t.started = true
	go func() {
		defer close(t.done)
		tk := time.NewTicker(flushInterval)
		defer tk.Stop()
		for {
			select {
			case <-t.stop:
				t.flush(true)
				return
			case <-tk.C:
				t.flush(false)
			}
		}
	}()
}

// Stop 停止后台循环并做最后一次落盘（幂等）。未 Start 时只落盘。
func (t *Tracker) Stop() {
	if t == nil {
		return
	}
	if !t.started {
		t.flush(true)
		return
	}
	t.stopOnce.Do(func() { close(t.stop) })
	<-t.done
}

func (t *Tracker) acctLocked(uid string) *account {
	a := t.accts[uid]
	if a == nil {
		a = &account{KindsHour: map[string]map[string]int64{}}
		t.accts[uid] = a
	}
	if a.KindsHour == nil {
		a.KindsHour = map[string]map[string]int64{}
	}
	return a
}

// hourLocked 取（必要时新建）当前小时桶，并裁掉窗口外的旧桶。
func (a *account) hourLocked(now time.Time) *hourBucket {
	key := now.Format(hourLayout)
	cut := now.Add(-(windowHours - 1) * time.Hour).Format(hourLayout)
	kept := a.Hours[:0]
	for _, b := range a.Hours {
		if b.Hour >= cut {
			kept = append(kept, b)
		}
	}
	a.Hours = kept
	for h := range a.KindsHour {
		if h < cut {
			delete(a.KindsHour, h)
		}
	}
	if n := len(a.Hours); n > 0 && a.Hours[n-1].Hour == key {
		return &a.Hours[n-1]
	}
	a.Hours = append(a.Hours, hourBucket{Hour: key})
	return &a.Hours[len(a.Hours)-1]
}

// Success 记一次成功的上游尝试。
func (t *Tracker) Success(uid string) {
	if t == nil || uid == "" {
		return
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.acctLocked(uid)
	a.hourLocked(now).OK++
	a.LastOK = now
	t.dirty = true
}

// Failure 记一次失败的上游尝试。kind 用 upstream.ErrKind.String() 或 KindTransport。
func (t *Tracker) Failure(uid, kind string, status int, model, msg string) {
	if t == nil || uid == "" {
		return
	}
	if kind == "" {
		kind = "unknown"
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.acctLocked(uid)
	a.hourLocked(now).Err++
	hk := now.Format(hourLayout)
	if a.KindsHour[hk] == nil {
		a.KindsHour[hk] = map[string]int64{}
	}
	a.KindsHour[hk][kind]++
	a.LastErr = &LastError{Time: now, Kind: kind, Status: status, Model: model, Msg: clip(msg)}
	t.dirty = true
}

// Balance 记一次余额查询结果（err == nil 为成功）。
func (t *Tracker) Balance(uid string, err error) {
	if t == nil || uid == "" {
		return
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.acctLocked(uid)
	if err == nil {
		a.BalanceOK = now
		a.BalanceErr = nil
	} else {
		a.BalanceErr = &LastError{Time: now, Kind: "balance", Msg: clip(err.Error())}
	}
	t.dirty = true
}

// Forget 删除账号记录（账号移除时调用）。
func (t *Tracker) Forget(uid string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.accts[uid]; ok {
		delete(t.accts, uid)
		t.dirty = true
	}
}

// clip 压缩空白并截断到 maxMsgRunes。
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= maxMsgRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxMsgRunes]) + "…"
}

// ---------------------------------------------------------------- 视图 ----

// KindCount 一种错误类型的计数。
type KindCount struct {
	Kind  string `json:"kind"`
	Count int64  `json:"count"`
}

// AccountView 单账号近 24 小时健康视图。
type AccountView struct {
	UID         string      `json:"uid"`
	OK24h       int64       `json:"ok_24h"`
	Err24h      int64       `json:"err_24h"`
	SuccessRate float64     `json:"success_rate"` // 0–100；无请求时为 -1
	Kinds24h    []KindCount `json:"kinds_24h,omitempty"`
	LastErr     *LastError  `json:"last_err,omitempty"`
	LastOK      time.Time   `json:"last_ok,omitempty"`
	BalanceOK   time.Time   `json:"balance_ok,omitempty"`
	BalanceErr  *LastError  `json:"balance_err,omitempty"`
	// Hourly 近 24 小时逐小时成功/失败（升序，缺失小时补 0），供面板画迷你柱。
	Hourly []HourPoint `json:"hourly"`
}

// HourPoint 一小时计数。
type HourPoint struct {
	Hour string `json:"hour"`
	OK   int64  `json:"ok"`
	Err  int64  `json:"err"`
}

// Summary 全池汇总。
type Summary struct {
	OK24h       int64         `json:"ok_24h"`
	Err24h      int64         `json:"err_24h"`
	SuccessRate float64       `json:"success_rate"`
	Kinds24h    []KindCount   `json:"kinds_24h,omitempty"`
	Hourly      []HourPoint   `json:"hourly"`
	Accounts    []AccountView `json:"accounts"`
	Generated   time.Time     `json:"generated"`
}

// Snapshot 生成全池视图。uids 非空时只包含这些账号（并为没有记录的账号补空行）。
func (t *Tracker) Snapshot(uids []string) Summary {
	now := time.Now()
	if t != nil {
		now = t.now()
	}
	hours := make([]string, windowHours)
	for i := 0; i < windowHours; i++ {
		hours[i] = now.Add(-time.Duration(windowHours-1-i) * time.Hour).Format(hourLayout)
	}
	sum := Summary{Generated: now, Hourly: emptyHourly(hours)}
	if t == nil {
		sum.SuccessRate = -1
		return sum
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(uids) == 0 {
		for uid := range t.accts {
			uids = append(uids, uid)
		}
	}
	sort.Strings(uids)
	totalKinds := map[string]int64{}
	for _, uid := range uids {
		v := AccountView{UID: uid, Hourly: emptyHourly(hours)}
		if a := t.accts[uid]; a != nil {
			idx := map[string]int{}
			for i, h := range hours {
				idx[h] = i
			}
			for _, b := range a.Hours {
				if i, ok := idx[b.Hour]; ok {
					v.Hourly[i].OK += b.OK
					v.Hourly[i].Err += b.Err
					sum.Hourly[i].OK += b.OK
					sum.Hourly[i].Err += b.Err
					v.OK24h += b.OK
					v.Err24h += b.Err
				}
			}
			kinds := map[string]int64{}
			for h, m := range a.KindsHour {
				if _, ok := idx[h]; !ok {
					continue
				}
				for k, n := range m {
					kinds[k] += n
					totalKinds[k] += n
				}
			}
			v.Kinds24h = sortedKinds(kinds)
			v.LastErr, v.LastOK = a.LastErr, a.LastOK
			v.BalanceOK, v.BalanceErr = a.BalanceOK, a.BalanceErr
		}
		v.SuccessRate = rate(v.OK24h, v.Err24h)
		sum.OK24h += v.OK24h
		sum.Err24h += v.Err24h
		sum.Accounts = append(sum.Accounts, v)
	}
	sum.Kinds24h = sortedKinds(totalKinds)
	sum.SuccessRate = rate(sum.OK24h, sum.Err24h)
	return sum
}

func emptyHourly(hours []string) []HourPoint {
	out := make([]HourPoint, len(hours))
	for i, h := range hours {
		out[i] = HourPoint{Hour: h}
	}
	return out
}

func rate(ok, bad int64) float64 {
	if ok+bad == 0 {
		return -1
	}
	return float64(int64(float64(ok)/float64(ok+bad)*1000+0.5)) / 10
}

func sortedKinds(m map[string]int64) []KindCount {
	out := make([]KindCount, 0, len(m))
	for k, n := range m {
		out = append(out, KindCount{Kind: k, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// ---------------------------------------------------------------- 持久化 ----

func (t *Tracker) load() error {
	raw, err := os.ReadFile(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f diskFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	if f.Accounts != nil {
		t.accts = f.Accounts
	}
	return nil
}

func (t *Tracker) flush(force bool) {
	if t == nil || t.path == "" {
		return
	}
	t.mu.Lock()
	if !t.dirty && !force {
		t.mu.Unlock()
		return
	}
	raw, err := json.Marshal(diskFile{Version: 1, Saved: time.Now().Format(time.RFC3339), Accounts: t.accts})
	t.dirty = false
	t.mu.Unlock()
	if err != nil {
		log.Printf("[health] 序列化失败: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
		log.Printf("[health] 建目录失败: %v", err)
		return
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[health] 写临时文件失败: %v", err)
		return
	}
	if err := os.Rename(tmp, t.path); err != nil {
		log.Printf("[health] 原子替换失败: %v", err)
	}
}
