package usage

import (
	"context"
	_ "embed"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 估算价值：把每个桶的 token 按模型公布单价折成美元，再按汇率折成人民币。
// 它回答的是「同样的 token 按 OpenRouter 公布价要花多少钱」，不是 WorkBuddy 的实际
// 结算（实际结算看积分）。
//
// 价格来源（优先级从高到低）：
//  1. 动态刷新：PriceRefresher 按间隔拉取 OpenRouter /api/v1/models，匹配后写入
//     data/pricing.json（重启后先用它）；
//  2. 内置快照 prices.json（取自 workbuddy2api-hub 的 OpenRouter 价格快照）。
//
// 拉取在后台进行，请求链路只读内存里的当前表（atomic 指针），从不等网络；拉取失败
// 沿用上一版，不会把表清空。
//
// 校准：pricing-settings.json 里的按模型系数（估算金额 × 系数，把 OpenRouter 的
// top-provider 参考价对到你认可的价）与美元汇率。缺失、非正数按 1.0 / 默认汇率。
//
// 计费口径：
//   - 输入按「缓存命中」与「未命中」分开计价：命中部分 = min(缓存命中, 输入)，
//     其余输入按未命中价；模型没公布命中价时按未命中价计（宁高勿低）。
//   - 输出只按 completion_tokens 计（推理 token 已含在其中，不重复计）。
//   - 按输入长度 / 时段分档的模型只取基础档：桶内是聚合量，拿不到单条请求的
//     输入长度和时间点，长上下文请求会被低估。
//   - 桶不记录当时生效的价格版本，所以历史用量一律按**当前**价格估算。
//   - 不在表内的模型不计价，单独列在 unpriced_models 里，不猜价格。

//go:embed prices.json
var pricesJSON []byte

// defaultUsdCny 汇率缺省值（与内置快照一致）。
const defaultUsdCny = 7.1

type modelPrice struct {
	Input    float64  `json:"input"`
	Output   float64  `json:"output"`
	CacheHit *float64 `json:"cache_hit,omitempty"`
	// ORID 匹配到的 OpenRouter 模型 id（动态刷新写入；内置快照为空）。
	ORID string `json:"or_id,omitempty"`
	// Via 匹配方式：exact（名字归一化唯一命中）/ override（内置或手填映射）/
	// variant（剥渠道后缀继承基准模型）；内置快照为空。
	Via string `json:"via,omitempty"`
}

type priceTable struct {
	Source      string                `json:"source"`
	GeneratedAt string                `json:"generated_at"`
	UsdCny      float64               `json:"usd_cny"`
	Unit        float64               `json:"unit"`
	Models      map[string]modelPrice `json:"models"`
}

// pricing 运行时生效的价格状态：基础表 + 校准设置。整体替换（atomic），读侧无锁。
type pricing struct {
	table   priceTable
	markup  map[string]float64 // 模型 → 校准系数（>0）
	usdCny  float64            // 生效汇率（设置优先，其次表内值）
	dynamic bool               // table 来自动态刷新（而非内置快照）
}

var current atomic.Pointer[pricing]

func init() {
	t := builtinPrices()
	current.Store(&pricing{table: t, usdCny: t.UsdCny})
}

func builtinPrices() priceTable {
	var t priceTable
	if err := json.Unmarshal(pricesJSON, &t); err != nil {
		log.Printf("[usage] 价格表解析失败（估算价值不可用）: %v", err)
	}
	return normalizeTable(t)
}

func normalizeTable(t priceTable) priceTable {
	if t.Unit <= 0 {
		t.Unit = 1_000_000
	}
	if t.UsdCny <= 0 {
		t.UsdCny = defaultUsdCny
	}
	if t.Models == nil {
		t.Models = map[string]modelPrice{}
	}
	return t
}

func pricesNow() *pricing { return current.Load() }

// markupOf 模型的校准系数（未设置为 1）。
func (p *pricing) markupOf(model string) float64 {
	if m, ok := p.markup[model]; ok && m > 0 {
		return m
	}
	return 1
}

// bucketCostUSD 按单价估算一个桶的美元价值（已乘校准系数）；ok=false 表示模型不在价格表内。
func bucketCostUSD(b *bucket) (float64, bool) {
	return pricesNow().bucketCostUSD(b)
}

func (pr *pricing) bucketCostUSD(b *bucket) (float64, bool) {
	model := canonicalUsageModel(b.Model)
	p, ok := pr.table.Models[model]
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
	usd := (float64(uncached)*p.Input + float64(cached)*hitRate + float64(b.CT)*p.Output) / pr.table.Unit
	return usd * pr.markupOf(model), true
}

// PriceInfo 面板展示价格口径用的元信息。
type PriceInfo struct {
	Source      string  `json:"source"`
	GeneratedAt string  `json:"generated_at"`
	UsdCny      float64 `json:"usd_cny"`
	// Dynamic 价格表来自 OpenRouter 动态刷新（false = 内置快照）。
	Dynamic bool `json:"dynamic"`
	// Calibrated 有模型设置了校准系数。
	Calibrated bool `json:"calibrated,omitempty"`
}

func priceInfo() PriceInfo {
	p := pricesNow()
	return PriceInfo{
		Source:      p.table.Source,
		GeneratedAt: p.table.GeneratedAt,
		UsdCny:      p.usdCny,
		Dynamic:     p.dynamic,
		Calibrated:  len(p.markup) > 0,
	}
}

func usdCnyNow() float64 { return pricesNow().usdCny }

// ---------------------------------------------------------------- 设置 ----

// PriceSettings 价格刷新与校准设置（data/pricing-settings.json）。
type PriceSettings struct {
	// RefreshEnabled 是否后台拉取 OpenRouter（默认开）。
	RefreshEnabled bool `json:"refresh_enabled"`
	// RefreshHours 拉取间隔（小时，默认 24，最小 1）。
	RefreshHours int `json:"refresh_hours"`
	// UsdCny 美元汇率；0 = 用价格表内的值。
	UsdCny float64 `json:"usd_cny,omitempty"`
	// Markup 按模型校准系数（估算金额 × 系数）。
	Markup map[string]float64 `json:"markup,omitempty"`
	// Mapping 手填映射：本地模型名 → OpenRouter 模型 id（优先于自动匹配）。
	Mapping map[string]string `json:"mapping,omitempty"`
}

// DefaultPriceSettings 缺省设置。
func DefaultPriceSettings() PriceSettings {
	return PriceSettings{RefreshEnabled: true, RefreshHours: 24}
}

// Normalize 清洗设置：间隔钳到 [1, 720] 小时，丢弃非正系数与空映射。
func (s PriceSettings) Normalize() PriceSettings {
	if s.RefreshHours <= 0 {
		s.RefreshHours = 24
	}
	if s.RefreshHours > 720 {
		s.RefreshHours = 720
	}
	if s.UsdCny < 0 {
		s.UsdCny = 0
	}
	markup := map[string]float64{}
	for k, v := range s.Markup {
		k = strings.TrimSpace(k)
		if k != "" && v > 0 && v != 1 {
			markup[k] = v
		}
	}
	s.Markup = markup
	mapping := map[string]string{}
	for k, v := range s.Mapping {
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k != "" && v != "" {
			mapping[k] = v
		}
	}
	s.Mapping = mapping
	return s
}

// ---------------------------------------------------------------- 刷新器 ----

// FetchFunc 拉取 OpenRouter 模型列表：id → 原始 pricing 字段（每 token 美元，字符串）。
// ctx 在 Stop 时取消，实现必须据此中止网络请求。
type FetchFunc func(ctx context.Context) (map[string]map[string]string, error)

// PriceRefresher 后台价格刷新与校准管理。
type PriceRefresher struct {
	dir   string // data 目录（pricing.json / pricing-settings.json 所在）
	fetch FetchFunc
	ids   func() []string // 需要定价的本地模型名（目录 + 已用过的模型）

	mu       sync.Mutex
	settings PriceSettings
	status   RefreshStatus
	running  bool
	kick     chan struct{}
	ctx      context.Context // Stop 时取消：中止进行中的拉取
	cancel   context.CancelFunc
	started  bool
	// startDelay 启动后首次拉取前的等待：不与进程启动抢资源，短命进程（测试）也不联网。
	startDelay time.Duration
	done       chan struct{}
	stopOnce   sync.Once
}

// RefreshStatus 最近一次拉取的结果（面板展示）。
type RefreshStatus struct {
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	Matched     int       `json:"matched"`
	Unmatched   []string  `json:"unmatched,omitempty"`
	Running     bool      `json:"running,omitempty"`
}

// NewPriceRefresher 创建刷新器：加载设置与上次的动态价格表并立即生效。
// ids 返回需要定价的本地模型名；fetch 为 nil 时用 FetchOpenRouter。
func NewPriceRefresher(dir string, ids func() []string, fetch FetchFunc) *PriceRefresher {
	if fetch == nil {
		fetch = FetchOpenRouter
	}
	r := &PriceRefresher{dir: dir, fetch: fetch, ids: ids,
		kick: make(chan struct{}, 1), done: make(chan struct{}), startDelay: 30 * time.Second}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.settings = r.loadSettings()
	if t, ok := r.loadTable(); ok {
		r.status.LastSuccess = parseGenerated(t.GeneratedAt)
		r.status.Matched = len(t.Models)
		r.apply(t, true)
	} else {
		r.apply(builtinPrices(), false)
	}
	return r
}

func (r *PriceRefresher) tablePath() string    { return filepath.Join(r.dir, "pricing.json") }
func (r *PriceRefresher) settingsPath() string { return filepath.Join(r.dir, "pricing-settings.json") }

func parseGenerated(s string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02T15:04:05", s, time.Local)
	return t
}

// apply 用基础表 + 当前设置组装生效状态。调用方可持或不持 r.mu（只读 settings 副本）。
func (r *PriceRefresher) apply(t priceTable, dynamic bool) {
	r.mu.Lock()
	s := r.settings
	r.mu.Unlock()
	t = normalizeTable(t)
	usd := t.UsdCny
	if s.UsdCny > 0 {
		usd = s.UsdCny
	}
	current.Store(&pricing{table: t, markup: s.Markup, usdCny: usd, dynamic: dynamic})
}

// Start 启动后台循环：立即检查一次是否到期，之后按间隔刷新；Refresh 可随时唤醒。
func (r *PriceRefresher) Start() {
	r.started = true
	go func() {
		defer close(r.done)
		if r.startDelay > 0 {
			t := time.NewTimer(r.startDelay)
			select {
			case <-r.ctx.Done():
				t.Stop()
				return
			case <-r.kick: // 面板手动刷新不必等启动缓冲
				t.Stop()
				r.refreshOnce()
			case <-t.C:
			}
		}
		for {
			r.mu.Lock()
			s, last := r.settings, r.status.LastSuccess
			r.mu.Unlock()
			wait := time.Duration(s.RefreshHours) * time.Hour
			if s.RefreshEnabled {
				if due := time.Until(last.Add(wait)); due <= 0 {
					r.refreshOnce()
				} else {
					wait = due
				}
			}
			t := time.NewTimer(wait)
			select {
			case <-r.ctx.Done():
				t.Stop()
				return
			case <-r.kick:
				t.Stop()
				r.refreshOnce()
			case <-t.C:
			}
		}
	}()
}

// Stop 停止后台循环并中止进行中的拉取（幂等）。
func (r *PriceRefresher) Stop() {
	r.stopOnce.Do(r.cancel)
	if r.started {
		<-r.done
	}
}

// Refresh 请求立即刷新一次（异步；已在刷新中则合并）。
func (r *PriceRefresher) Refresh() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// RefreshNow 同步刷新一次（面板「立即刷新」与测试用）。
func (r *PriceRefresher) RefreshNow() RefreshStatus {
	r.refreshOnce()
	return r.Status()
}

func (r *PriceRefresher) refreshOnce() {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.status.Running = true
	r.status.LastAttempt = time.Now()
	mapping := r.settings.Mapping
	r.mu.Unlock()

	or, err := r.fetch(r.ctx)
	var t priceTable
	var unmatched []string
	if err == nil {
		t, unmatched = BuildPriceTable(or, r.modelIDs(), mapping)
		if len(t.Models) == 0 {
			err = errEmptyMatch
		}
	}

	r.mu.Lock()
	r.running = false
	r.status.Running = false
	if r.ctx.Err() != nil {
		r.mu.Unlock()
		return // 关停中：不记失败、不落盘
	}
	if err != nil {
		r.status.LastError = err.Error()
		r.mu.Unlock()
		log.Printf("[pricing] OpenRouter 价格刷新失败（沿用当前价格）: %v", err)
		return
	}
	r.status.LastError = ""
	r.status.LastSuccess = time.Now()
	r.status.Matched = len(t.Models)
	r.status.Unmatched = unmatched
	r.mu.Unlock()

	r.apply(t, true)
	if err := writeJSONAtomic(r.tablePath(), t); err != nil {
		log.Printf("[pricing] 保存价格表失败: %v", err)
	}
	log.Printf("[pricing] OpenRouter 价格已刷新：匹配 %d 个模型，未匹配 %d 个", len(t.Models), len(unmatched))
}

type pricingError string

func (e pricingError) Error() string { return string(e) }

const errEmptyMatch = pricingError("OpenRouter 返回的模型一个都没匹配上（沿用当前价格）")

// modelIDs 需要定价的本地模型：内置快照的模型 ∪ 调用方给的（目录 + 用过的）。
func (r *PriceRefresher) modelIDs() []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		id = canonicalUsageModel(id)
		if id == "(unknown)" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for id := range builtinPrices().Models {
		add(id)
	}
	if r.ids != nil {
		for _, id := range r.ids() {
			add(id)
		}
	}
	sort.Strings(out)
	return out
}

// Status 最近一次刷新状态。
func (r *PriceRefresher) Status() RefreshStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.status
	s.Unmatched = append([]string(nil), s.Unmatched...)
	return s
}

// Settings 当前设置副本。
func (r *PriceRefresher) Settings() PriceSettings {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.settings
	s.Markup = copyMap(s.Markup)
	s.Mapping = copyMap(s.Mapping)
	return s
}

// SetSettings 保存设置并立即生效（校准与汇率立刻反映到估算；映射在下次刷新生效，
// 映射有变化时自动触发一次刷新）。
func (r *PriceRefresher) SetSettings(s PriceSettings) (PriceSettings, error) {
	s = s.Normalize()
	if err := writeJSONAtomic(r.settingsPath(), s); err != nil {
		return PriceSettings{}, err
	}
	r.mu.Lock()
	old := r.settings
	r.settings = s
	r.mu.Unlock()
	cur := pricesNow()
	r.apply(cur.table, cur.dynamic)
	if !equalMap(old.Mapping, s.Mapping) || (s.RefreshEnabled && !old.RefreshEnabled) || s.RefreshHours != old.RefreshHours {
		r.Refresh()
	}
	return s, nil
}

func (r *PriceRefresher) loadSettings() PriceSettings {
	s := DefaultPriceSettings()
	raw, err := os.ReadFile(r.settingsPath())
	if err != nil {
		return s
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		log.Printf("[pricing] 设置文件损坏，按缺省处理: %v", err)
		return DefaultPriceSettings()
	}
	return s.Normalize()
}

func (r *PriceRefresher) loadTable() (priceTable, bool) {
	raw, err := os.ReadFile(r.tablePath())
	if err != nil {
		return priceTable{}, false
	}
	var t priceTable
	if err := json.Unmarshal(raw, &t); err != nil || len(t.Models) == 0 {
		return priceTable{}, false
	}
	return t, true
}

// PriceRow 面板价格表一行。
type PriceRow struct {
	Model    string   `json:"model"`
	Input    float64  `json:"input"`
	Output   float64  `json:"output"`
	CacheHit *float64 `json:"cache_hit,omitempty"`
	ORID     string   `json:"or_id,omitempty"`
	Via      string   `json:"via,omitempty"`
	Markup   float64  `json:"markup"`
}

// Rows 当前生效价格表（按模型名排序）。
func (r *PriceRefresher) Rows() []PriceRow {
	p := pricesNow()
	out := make([]PriceRow, 0, len(p.table.Models))
	for m, v := range p.table.Models {
		out = append(out, PriceRow{Model: m, Input: v.Input, Output: v.Output, CacheHit: v.CacheHit,
			ORID: v.ORID, Via: v.Via, Markup: p.markupOf(m)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// Info 当前价格口径。
func (r *PriceRefresher) Info() PriceInfo { return priceInfo() }

func copyMap[V any](m map[string]V) map[string]V {
	out := make(map[string]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func equalMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func writeJSONAtomic(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
