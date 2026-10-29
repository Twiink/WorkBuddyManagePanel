// Package gateway 提供网关的可复用启动、装配与优雅停机。
//
// 命令行和桌面应用共享这一份生命周期。端口在 Start 返回前已经绑定，因此调用方
// 可以放心打开面板；重复启动、端口冲突和配置错误都会作为错误返回。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/buildinfo"
	"github.com/linguo2625469/workbuddy2api-panel/internal/credithist"
	"github.com/linguo2625469/workbuddy2api-panel/internal/health"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/panel"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// Options 控制一次网关实例的启动。
type Options struct {
	ConfigPath string
	// DataDir 是相对 auth_dir、state_file 和 prompt.file 的基准目录。
	// 为空时保留 CLI 的原有语义（相对当前工作目录）。
	DataDir string
	// Listen 非空时覆盖配置文件和 WB2A_LISTEN。
	Listen string
	// RequireLoopback 供桌面版强制只监听本机回环地址。
	RequireLoopback bool
	Version         string
}

// Instance 是一个已经完成端口绑定并开始运行的网关实例。
type Instance struct {
	URL     string
	Config  *Config
	URLPath string
	// DesktopHandler 仅挂到 Wails 原生资源通道，不能挂到 HTTP 监听器。
	DesktopHandler http.Handler

	server     *http.Server
	listener   net.Listener
	cancel     context.CancelFunc
	closeOnce  sync.Once
	closeErr   error
	wg         sync.WaitGroup
	p          *pool.Pool
	store      redisstore.Store
	usage      *usage.Recorder
	requestLog *reqlog.Recorder
	health     *health.Tracker
	prices     *usage.PriceRefresher
	logStore   *logstore.Store
	session    *session.Router
}

// UsagePathFor 由 state 文件路径推出用量文件路径：同目录、文件名 usage.json。
func UsagePathFor(stateFile string) string { return StateSibling(stateFile, "usage.json") }

// StateSibling 返回与 state 文件同目录的指定文件名路径（相对路径场景回落当前目录）。
func StateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

// Close 停止接收请求、取消调度，并按依赖逆序刷新状态。
// 可重复调用；首次完整的停机结束后，其余调用会返回同一结果。
func (i *Instance) Close() error {
	if i == nil {
		return nil
	}
	i.closeOnce.Do(i.shutdown)
	return i.closeErr
}

func (i *Instance) shutdown() {
	if i.cancel != nil {
		i.cancel()
	}
	if i.server != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := i.server.Shutdown(shutdownCtx); err != nil {
			i.closeErr = fmt.Errorf("shutdown http server: %w", err)
		}
		cancel()
	}
	if i.listener != nil {
		_ = i.listener.Close()
	}
	i.wg.Wait()
	if i.session != nil {
		i.session.StopGC()
	}
	if i.requestLog != nil {
		i.requestLog.Close()
	}
	if i.logStore != nil {
		i.logStore.Close()
	}
	if i.prices != nil {
		i.prices.Stop()
	}
	if i.health != nil {
		i.health.Stop()
	}
	if i.usage != nil {
		i.usage.Stop()
	}
	if i.p != nil {
		i.p.Close()
	}
	if i.store != nil {
		if err := i.store.Close(); err != nil && i.closeErr == nil {
			i.closeErr = fmt.Errorf("close redis store: %w", err)
		}
	}
	log.SetOutput(os.Stderr)
	server.SetChatLogOutput(os.Stdout)
}

// Start 加载配置、绑定端口并启动后台组件。返回时端口已可接受请求。
func Start(ctx context.Context, opts Options) (*Instance, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.ConfigPath == "" {
		opts.ConfigPath = "config.json"
	}
	if opts.Version == "" {
		opts.Version = buildinfo.Version
	}
	cfg, err := loadConfig(opts.ConfigPath, opts.DataDir)
	if err != nil {
		// 只有主配置缺失才走首次生成；引用的提示词缺失应直接报错。
		var readErr *configReadError
		if errors.As(err, &readErr) && errors.Is(readErr, fs.ErrNotExist) {
			// 首次运行：目录下没有配置 → 自动落一份推荐配置再加载。
			// 独立 server 保持原行为：生成随机密钥并要求输入后才能进入面板。
			// 桌面版则保留空密钥，由内嵌窗口先显示设置页，让用户自行输入或
			// 点击生成密钥，避免悄悄替用户生成一个“登录令牌”。
			var werr error
			if opts.RequireLoopback {
				werr = WriteDesktopDefault(opts.ConfigPath)
			} else {
				_, werr = WriteDefault(opts.ConfigPath)
			}
			if werr == nil {
				log.Printf("config %s 不存在，已生成推荐配置", opts.ConfigPath)
				cfg, err = loadConfig(opts.ConfigPath, opts.DataDir)
			}
			if err != nil {
				// 生成失败（目录只读等）：退回纯默认 + env（旧行为兜底），不阻塞启动。
				log.Printf("config %s not found (auto-generate failed), using defaults+env: %v", opts.ConfigPath, err)
				cfg, err = loadConfig("", opts.DataDir)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("load config: %w", err)
		}
	}
	resolveConfigPaths(cfg, opts.DataDir)
	if opts.Listen != "" {
		cfg.Listen = opts.Listen
	}
	if opts.RequireLoopback {
		normalized, err := normalizeLoopbackListen(cfg.Listen)
		if err != nil {
			return nil, err
		}
		cfg.Listen = normalized
		if err := requireLoopbackListen(cfg.Listen); err != nil {
			return nil, err
		}
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", cfg.Listen, err)
	}
	i := &Instance{Config: cfg, listener: listener}
	runCtx, cancel := context.WithCancel(ctx)
	i.cancel = cancel
	instance, err := finishStart(runCtx, i, opts)
	if err != nil {
		_ = i.Close()
		return nil, err
	}
	if err := runCtx.Err(); err != nil {
		_ = i.Close()
		return nil, err
	}
	return instance, nil
}

// resolveConfigPaths 把桌面数据目录中的相对路径锚定到同一目录。
func resolveConfigPaths(cfg *Config, dataDir string) {
	if dataDir == "" {
		return
	}
	cfg.AuthDir = anchorPath(cfg.AuthDir, dataDir)
	cfg.StateFile = anchorPath(cfg.StateFile, dataDir)
	cfg.Prompt.File = anchorPath(cfg.Prompt.File, dataDir)
	cfg.Upstream.DeviceTokenFile = anchorPath(cfg.Upstream.DeviceTokenFile, dataDir)
}

func anchorPath(path, base string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}

func requireLoopbackListen(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("desktop listen address %q: %w", listen, err)
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("desktop mode only allows a loopback listen address, got %q", listen)
	}
	return nil
}

// normalizeLoopbackListen 把配置中常见的 ":7863" 形式补成显式回环地址。
// 仅桌面模式调用；浏览器/服务端部署仍保留原有监听语义。
func normalizeLoopbackListen(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("desktop listen address %q: %w", listen, err)
	}
	if host != "" && host != "localhost" {
		ip := net.ParseIP(strings.Trim(host, "[]"))
		if ip == nil || !ip.IsLoopback() {
			return "", fmt.Errorf("desktop mode only allows a loopback listen address, got %q", listen)
		}
	}
	if port == "" {
		return "", fmt.Errorf("desktop listen port is empty")
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return "", fmt.Errorf("desktop listen port %q: %w", port, err)
	}
	if host != "" {
		return listen, nil
	}
	return net.JoinHostPort("127.0.0.1", port), nil
}

func finishStart(
	ctx context.Context,
	i *Instance,
	opts Options,
) (*Instance, error) {
	cfg := i.Config
	cfgPath, version, requireLoopback := opts.ConfigPath, opts.Version, opts.RequireLoopback

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("load auths: %w", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)
	i.store = store

	p := pool.New(cfg.StateFile)
	i.p = p
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限（含 global 分档）+ 连败降权 + 闲置补偿调优（从 config 注入，
	// 非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal) // global 域 WAF 风控分档（P1-1）
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(cfg.SoftRateMaxDur)                 // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur) // costTier 探索窗口（issue #136，默认 30m；0 关停）
	p.SetCreditFloor(cfg.Pool.CreditFloor)               // 积分保底（默认 0 = 关闭）
	p.SetDailyLimits(dailyLimitsOf(cfg))                 // 账号级每日护栏（默认全关）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetPreferExpiring(cfg.Pool.PreferExpiring)
	p.SetCrossDomainFallback(cfg.Pool.CrossDomainFallback) // 裸名跨域回退（issue #135，默认关闭）
	_ = p.SetScheduling(cfg.Pool.Strategy, cfg.Pool.AccountOrder)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏）；
			// 裸名走 cn（现状零回归）。闭包内部 resolveModel 剥前缀，再按 realm 过滤。
			AvailableForModel: realmAwareAvailableForModel(p),
			PreferredForModel: realmAwareExpiringForModel(p),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
	}
	i.session = sessRouter
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()

	// 积分保底的「收费」兜底判据：接上游模型目录的积分倍率表。本地实测台账无观测
	// 时用它判收费——否则「没学过」恒等于「放行」，高价新模型会把触底号一笔打穿
	// （kimi-k3-1 实案：全池无观测 → 保底全放行 → 两笔打穿并硬冷却到次日 04:00）。
	// 位于 up 装配之后：倍率表由探测下发，闭包每次调用读实时快照。
	p.SetModelRateOf(func(realm, model string) string { return up.ModelRate(realm, model) })

	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints.Store(cfg.Features.SanitizeBlacklistFingerprints)
	// 出站 UA 与归属头（issue #42 + 上游同步）：
	// UserAgent 非空则完全覆盖；ClientVersion/CliVersion 缺省对齐官方形态；
	// ClientName 非空时 chat 路径注入 X-IDE-* 四头（用量归因对齐官方桌面端）。
	up.UserAgent = cfg.Upstream.UserAgent
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	up.ClientName = cfg.Upstream.ClientName
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm 路由（config global 段）：上游侧开关（第一道闸）+ base 覆盖；
	// auth 侧开关（auth.SetGlobalEnabled）是第二道闸，两者同 config global.enabled。
	up.GlobalEnabled = cfg.Global.Enabled
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	applyCNChatBase(up, cfg)
	auth.SetGlobalEnabled(cfg.Global.Enabled)
	// DNS：Android 没有 /etc/resolv.conf，Go 的纯 Go 解析器会退回 127.0.0.1/::1，
	// 于是查任何域名都变成"连本机 53 端口"（dial tcp: lookup copilot.tencent.com
	// on [::1]:53: connection refused）。配了 dns_servers / WB2A_DNS 就显式指定。
	if len(cfg.DNSServers) > 0 {
		d := upstream.NewDNSDialer(cfg.DNSServers)
		up.SetDNSDialer(d)
		// 推送 webhook / Upstash 镜像等走 http.DefaultTransport，一并换掉
		if t, ok := http.DefaultTransport.(*http.Transport); ok {
			t.DialContext = d.DialContext
		}
		log.Printf("DNS: 出站解析用 %v（不走系统 /etc/resolv.conf）", cfg.DNSServers)
	}
	// model.json 本地缓存接线（context_length/max_output_tokens 四级查找链第 3 级）：
	// 数据目录与 state.json 同风格（Docker volume 持久化路径）。首次缺失/损坏自动
	// 回落仓库内嵌种子；models.dev 按需拉取成功后原子写回。
	upstream.SetModelCatalogPath(StateSibling(cfg.StateFile, "model.json"))

	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   cfg.Schedule.CheckinHours,
		TravelHours:    cfg.Schedule.TravelHours,
		ActivityHours:  cfg.Schedule.ActivityHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
		BlackcatHours:  cfg.Schedule.BlackcatHours,
		GrowthHours:    cfg.Schedule.GrowthHours,
		// 快过期积分优先消耗：签到/余额刷新按此窗口分桶（issue:积分过期）。
		ExpiringSoonWindow: cfg.ExpiringSoonDur,
		CheckinDisabled:    !cfg.Schedule.CheckinEnabled,
		TravelDisabled:     !cfg.Schedule.TravelEnabled,
		ActivityDisabled:   !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:  !cfg.Schedule.KeepaliveEnabled,
		BlackcatDisabled:   !cfg.Schedule.BlackcatEnabled,
		GrowthDisabled:     !cfg.Schedule.GrowthEnabled,
		// 保号类四任务是否覆盖禁用账号（缺省 false = 禁用即跳过，保持既有行为）。
		IncludeDisabledInTasks: cfg.Schedule.IncludeDisabledInTasks,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每日 1 次，点亮连登 + 解锁 first_buddy）", cfg.Schedule.ActivityHours)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	switch {
	case !cfg.Schedule.BlackcatEnabled:
		log.Printf("夜猫子已禁用（schedule.blackcat_enabled=false）")
	default:
		log.Printf("夜猫子已启用：%v 点（23:00–08:00 窗口 glm-5.2 对话补足）", cfg.Schedule.BlackcatHours)
	}
	switch {
	case !cfg.Schedule.BalanceRefreshEnabled:
		log.Printf("余额后台刷新已禁用（schedule.balance_refresh_enabled=false）")
	case cfg.BalanceRefreshInterval > 0:
		log.Printf("余额后台刷新：每 %s（签到时点照常额外刷新）", cfg.BalanceRefreshInterval)
	}
	switch {
	case !cfg.Schedule.TaskSweepEnabled:
		log.Printf("任务补领清扫已禁用（schedule.task_sweep_enabled=false）")
	case cfg.TaskSweepInterval > 0:
		log.Printf("任务补领清扫：每 %s（补领已达标未领的成长任务奖励）", cfg.TaskSweepInterval)
	}
	if cfg.Schedule.IncludeDisabledInTasks {
		log.Printf("保号任务覆盖禁用账号（schedule.include_disabled_in_tasks=true）：禁用号仍签到 / 活跃 / 保活 / 刷新余额，但不参与选号")
	}

	// 管理面板日志镜像：标准 log（stderr）与 chat 表格日志（stdout）双路复制进
	// 面板环形缓冲，供 /panel/api/logs 读取；控制台输出行为完全不变。
	// live 承载可热改字段（api_key/soft_rate/脱敏开关），面板保存配置时在线替换。
	live := livecfg.New(livecfg.Snapshot{
		APIKey:               cfg.APIKey,
		SoftCooldown:         cfg.SoftRateDur,
		SanitizeFingerprints: cfg.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     cfg.Logging.RequestClientInfo,
		HiddenModels:         livecfg.NormalizeHiddenModels(cfg.Models.Hidden),
		BareModelNames:       cfg.Models.BareNames,
		PreferredRealm:       cfg.Models.PreferredRealm,
	})
	// 用量记录器：与 state 文件同目录，随 state_file 配置一起搬移。
	// datapath 由 state 文件路径推出，避免再加一个配置项。
	usagePath := UsagePathFor(cfg.StateFile)
	rec := usage.New(usagePath)
	i.usage = rec
	rec.Start()
	// 每日护栏计数来源：用量记录器的当日索引（选号热路径 O(1) 查询）。
	p.SetDailyUsage(func(now time.Time, uid, model string) pool.DailyCount {
		d := rec.Daily(now, uid, model)
		return pool.DailyCount{Tokens: d.Tokens, Credits: d.Credits, ModelTokens: d.ModelTokens}
	})
	p.SetDailyModels(rec.DailyModels)
	log.Printf("[usage] 逐请求用量记录已启用: %s (%s)", usagePath, rec.Describe())

	// OpenRouter 动态价格：后台按间隔刷新（默认 24h），失败沿用上一版；请求链路只读
	// 内存中的当前表。需要定价的模型 = 内置快照 ∪ 上游目录 ∪ 用过的模型。
	priceRef := usage.NewPriceRefresher(filepath.Dir(usagePath), func() []string {
		return append(up.KnownModels(), rec.Models()...)
	}, nil)
	priceRef.Start()
	i.prices = priceRef

	// 请求指标始终启用；JSONL 归档只写脱敏元数据，写盘失败不影响聊天请求。
	requestLog := reqlog.New(reqlog.Config{
		Dir:           StateSibling(cfg.StateFile, "request-logs"),
		Enabled:       cfg.Logging.RequestArchiveEnabled,
		RetentionDays: cfg.Logging.RequestRetentionDays,
		MaxBytes:      int64(cfg.Logging.RequestArchiveMaxMB) << 20,
	})
	i.requestLog = requestLog
	rs := requestLog.Snapshot().Archive
	if rs.Enabled {
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档 %s（保留 %d 天，上限 %d MiB）",
			rs.Dir, cfg.Logging.RequestRetentionDays, cfg.Logging.RequestArchiveMaxMB)
	} else {
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档已关闭")
	}

	// SQLite 存储：请求记录 + 运行日志，供面板检索与导出（缺省关，面板开关热生效）。
	// 写入是非阻塞投递，队列满即丢弃计数；与 state 同目录。
	logStore := logstore.New(StateSibling(cfg.StateFile, "logs.sqlite3"))
	if err := logStore.Configure(sqliteSettingsOf(cfg)); err != nil {
		log.Printf("[logstore] SQLite 存储打开失败（检索/导出不可用）: %v", err)
	} else if logStore.Enabled() {
		log.Printf("[logstore] SQLite 存储已启用: %s", logStore.Path())
	}
	i.logStore = logStore
	requestLog.SetSink(logStore.AppendRequest)

	// 积分历史：把每次真实查到的余额与上一次比对，变动即留痕（新账号首次只建
	// 基线）。挂载在余额查询路径上（upstream 的余额观察者），签到 / 活跃上报 /
	// 旅行 / 保活 / 面板手动刷新全覆盖——上游只在少数渠道落金额日志，比对余额
	// 是唯一可靠的口径。与 state/usage/request-logs 同目录，随 state_file 搬移。
	creditHist := credithist.New(StateSibling(cfg.StateFile, "credit-history.json"), 2000)
	up.SetCreditObserver(creditHist.Observe)
	log.Printf("[credithist] 积分历史已启用: %s", StateSibling(cfg.StateFile, "credit-history.json"))

	// 账号健康台账：近 24h 成功/失败、错误类型分布、最近错误、余额查询成败。
	// 旁路记录（请求处置不读它），与 state 同目录。
	healthTracker := health.New(StateSibling(cfg.StateFile, "health.json"))
	healthTracker.Start()
	i.health = healthTracker
	up.SetBalanceResultObserver(healthTracker.Balance)

	pn := panel.New(panel.Config{
		Pool:        p,
		Usage:       rec,
		RequestLog:  requestLog,
		Health:      healthTracker,
		Prices:      priceRef,
		Upstream:    up,
		Scheduler:   sch,
		AuthDir:     cfg.AuthDir,
		APIKey:      cfg.APIKey,
		DesktopMode: requireLoopback,
		RedisMode:   redisMode,
		StickyCount: sessCount,
		Version:     version,
		Live:        live,
		// 模型上限探测数据（scripts/probe_max_tokens.py --panel-out 写入）：
		// 与 state 文件同目录，缺省 data/output_probes.json。
		ProbeFile:  StateSibling(cfg.StateFile, "output_probes.json"),
		ConfigPath: cfgPath,
		LoadConfig: func() (any, error) {
			return loadConfig(cfgPath, opts.DataDir)
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			restart, err := saveConfig(raw, cfgPath, live, p, up, sch, opts)
			if err == nil {
				// SQLite 开关与上限热生效：落盘成功后按新配置重新配置存储。
				if nc, lerr := loadConfig(cfgPath, opts.DataDir); lerr == nil {
					if cerr := logStore.Configure(sqliteSettingsOf(nc)); cerr != nil {
						log.Printf("[logstore] 应用 SQLite 设置失败: %v", cerr)
					}
				}
			}
			return restart, err
		},
		// 积分历史账本（只读展示；写入由上面的余额观察者完成）。
		CreditHistory: creditHist,
		LogStore:      logStore,
	})
	if requireLoopback {
		i.DesktopHandler = pn.DesktopHandler()
	}
	// 成长任务队列每日自动执行（与「执行全部待办」同管线）：Sequential 族零点解锁后
	// 无需手动扫描；hook 返回即启动（异步执行），已在跑时内部跳过。
	sch.SetGrowthHook(pn.RunGrowthQueueOnce)
	// 清扫回调返回统计汇总（测试/日志可观测），调度器 hook 只要求 func()：
	// 这里收掉返回值——汇总已由 panel 侧内部日志输出。
	sch.SetTaskSweepHook(func() { pn.RunTaskSweepOnce() })
	pn.Logs().SetSink(func(e panel.LogEntry) { logStore.AppendLog(e.TS, e.Ch, e.Text) })
	log.SetOutput(io.MultiWriter(os.Stderr, pn.Logs()))
	server.SetChatLogOutput(io.MultiWriter(os.Stdout, pn.Logs()))

	h := server.NewHandler(server.Config{
		Pool:          p,
		Upstream:      up,
		APIKey:        cfg.APIKey,
		RequireAPIKey: requireLoopback,
		Session:       sessRouter,
		StickyCount:   sessCount,
		RedisMode:     redisMode,
		SoftCooldown:  cfg.SoftRateDur,
		Panel:         pn,
		Live:          live,
		Usage:         rec,
		RequestLog:    requestLog,
		Health:        healthTracker,
		PromptMode:    cfg.Prompt.Mode,
		PromptText:    cfg.PromptText,
		// 来源记录开关经 livecfg 热生效；此处同时填静态字段，供 Live 为 nil 的
		// 裸用/测试路径拿到同一缺省值。
		RecordClientInfo: cfg.Logging.RequestClientInfo,
		// handler 侧第三道闸（global realm）：false（显式逃生门）时不列 global: 模型名。
		GlobalEnabled: cfg.Global.Enabled,
		// 模型黑名单原始条目；运行期以 livecfg 快照为准（热生效），此处供
		// Live 为 nil 的裸用/测试路径。
		ModelsHidden: cfg.Models.Hidden,
	})

	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		sch.Run(ctx)
	}()
	sch.StartBalanceRefresh(ctx, cfg.BalanceRefreshInterval)
	sch.StartTaskSweep(ctx, cfg.TaskSweepInterval)

	// 启动即预热模型积分倍率表：倍率只在 FetchModels/FetchGlobalModelInfos 成功时
	// 填充（两者均懒触发），重启后到首次 /v1/models 或面板模型页被访问之前，
	// ModelRate 恒返回空串——积分保底的目录兜底在这段空窗期内形同虚设，触底号
	// 会被当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费
	// 模型归零；倍率表当时尚未建立）。
	// 异步执行：不阻塞监听启动；失败仅记日志（下一轮懒触发或本轮重试仍可补上）。
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		warmModelRates(ctx, up, p)
	}()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body 上传）：防慢速 body 拖死连接。
		// 请求体已无网关侧上限（max_body_mb 移除）。缺省 300s（issue #100：旧固定
		// 60s 会掐掉大上下文/文件块经反代链的慢速上传，客户端收到
		// 400 "read body: ... i/o timeout"）；server.read_timeout="0" 显式关闭。
		// 改动需重启进程。
		ReadTimeout: cfg.ServerReadTimeoutDur,
		// IdleTimeout keep-alive 空闲连接回收：配合 chat 出站 ctx 传播防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	i.server = srv

	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		if err := srv.Serve(i.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http server stopped unexpectedly: %v", err)
			i.cancel()
		}
	}()

	host, port, splitErr := net.SplitHostPort(i.listener.Addr().String())
	if splitErr != nil {
		return nil, fmt.Errorf("resolve listener address: %w", splitErr)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	i.URL = "http://" + net.JoinHostPort(host, port) + "/panel/"
	i.URLPath = "/panel/"
	log.Printf("%s listening on %s (api_key=%v)，管理面板 %s", buildinfo.DesktopName, cfg.Listen, cfg.APIKey != "", i.URL)
	return i, nil
}

// warmModelRates 启动预热各域模型积分倍率表（供积分保底的目录兜底判定）。
//
// 为什么需要：倍率表只在 FetchModels（CN）/ FetchGlobalModelInfos（global）成功时
// 填充，两者都是懒触发（被 /v1/models 或面板模型页访问才跑）。重启后到首次触发
// 之间的空窗期里 ModelRate 恒返回空串，保底的目录兜底判不出收费，触底号会被
// 当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费模型归零）。
//
// 失败处理：单域失败只记 WARN（不阻塞、不致命——后续懒触发仍会补上）；global 域
// 仅在其路由开关开启时预热（逃生门关锁时按 CN 处理，无需探测）。
func warmModelRates(ctx context.Context, up *upstream.Client, p *pool.Pool) {
	// 预热不得拖住进程退出：ctx 取消（SIGINT/SIGTERM）时立刻放弃剩余域。
	if ctx.Err() != nil {
		return
	}
	// CN：有可用 CN 账号才拉（与面板 models 同口径，避免无谓上游调用）。
	if uids := p.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if a := p.AuthByUID(uids[0]); a != nil {
			if _, err := up.FetchModelsContext(ctx, a); err != nil {
				log.Printf("WARN: [upstream] warm model rates (cn): %v", err)
			} else {
				log.Printf("[upstream] warm model rates: cn ok")
			}
		}
	}
	// global：独立目录端点（workbuddy.ai），倍率按 "global" 域键存储。
	if up.GlobalEnabled && ctx.Err() == nil {
		if uids := p.AvailableUIDsForRealm("global"); len(uids) > 0 {
			if a := p.AuthByUID(uids[0]); a != nil {
				// FetchGlobalModelInfos 无错误返回（内部负缓存自行节流），
				// 仅按结果条数判断是否拿到目录。
				if infos := up.FetchGlobalModelInfosContext(ctx, a); len(infos) == 0 {
					log.Printf("WARN: [upstream] warm model rates (global): empty model list")
				} else {
					log.Printf("[upstream] warm model rates: global ok (%d models)", len(infos))
				}
			}
		}
	}
}

// panelListenPath 从 listen 地址提取 ":port" 形式，用于启动日志拼面板 URL
// （":7863" 或 "0.0.0.0:7863" → ":7863"；异常输入原样返回）。
func panelListenPath(listen string) string {
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			return listen[i:]
		}
	}
	return listen
}

// saveConfig 面板保存配置：校验 → 落盘 → 热应用 → 返回需重启的字段列表。
//
// 热生效范围（设计取舍）：
//   - api_key / cooldown.soft_rate / features.sanitize_blacklist_fingerprints → livecfg 快照
//   - pool.* → pool.SetBreaker/SetMaxInFlight/SetSoftRateMax/SetWeights/SetCostExploreInterval/SetPreferExpiring/SetCreditFloor
//   - schedule.* → scheduler.Reconfigure/SetBalanceInterval/SetExpiringSoonWindow
//
// 需重启（涉及监听地址、HTTP client 超时、auth_dir 等装配期依赖）：
//   - listen / auth_dir / state_file / upstream.* / upstash.* / session_sticky.*（TTL 类）
//
// 落盘用"先写 tmp 再 rename"原子替换，且优先保留磁盘上的原始 JSON 结构（只改
// 面板表单覆盖到的键），避免把用户手写的注释性字段/未知键洗掉——这里直接整体
// 序列化校验后的配置，未知键在 json.Unmarshal 时已丢失，故先合并原始 map。
func saveConfig(raw []byte, path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler, options ...Options) ([]string, error) {
	var opts Options
	if len(options) > 0 {
		opts = options[0]
	}
	desktopMode := opts.RequireLoopback
	if desktopMode {
		var incoming map[string]any
		if err := json.Unmarshal(raw, &incoming); err != nil {
			return nil, fmt.Errorf("parse submitted config: %w", err)
		}
		if listenValue, ok := incoming["listen"]; ok {
			listen, isString := listenValue.(string)
			if !isString {
				return nil, fmt.Errorf("desktop listen address must be a string")
			}
			if _, _, err := net.SplitHostPort(listen); err != nil {
				return nil, fmt.Errorf("desktop listen address %q: %w", listen, err)
			}
		}
	}
	// 1) 解析原始 JSON 为 map（保留用户手写的未知键），再叠加面板提交的键。
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read current config: %w", err)
	}
	var cur, incoming map[string]any
	if err := json.Unmarshal(oldRaw, &cur); err != nil {
		cur = map[string]any{}
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("parse submitted config: %w", err)
	}
	merged := mergeConfigMaps(cur, incoming)

	// 2) 校验（与启动同一套 Default+normalize），失败直接返回、不落盘。
	newCfg, err := parseConfigInto(mergedJSON(merged), Default(), opts.DataDir)
	if err != nil {
		return nil, err
	}
	if desktopMode {
		normalized, err := normalizeLoopbackListen(newCfg.Listen)
		if err != nil {
			return nil, err
		}
		newCfg.Listen = normalized
		if incomingListen, ok := incoming["listen"].(string); ok {
			if _, port, err := net.SplitHostPort(incomingListen); err == nil && strings.HasPrefix(incomingListen, ":") {
				merged["listen"] = net.JoinHostPort("127.0.0.1", port)
			}
		}
		if err := requireLoopbackListen(newCfg.Listen); err != nil {
			return nil, err
		}
	}

	// 3) 落盘（原子替换）。
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// A single-file Docker bind mount cannot be renamed over its mount
		// target (Linux returns EBUSY / "device or resource busy"). Keep the
		// atomic path for regular files, but update the mounted file in place
		// for this specific deployment shape.
		if !errors.Is(err, syscall.EBUSY) {
			return nil, fmt.Errorf("replace config: %w", err)
		}
		f, openErr := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
		if openErr != nil {
			_ = os.Remove(tmp)
			return nil, fmt.Errorf("replace config (bind mount fallback): %w", openErr)
		}
		_, writeErr := f.Write(out)
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		// 写失败时保留 tmp（挂载文件已被 O_TRUNC 破坏，tmp 里是完整新内容，
		// 可手工恢复）；写成功才清理。
		if writeErr != nil {
			return nil, fmt.Errorf("replace config (bind mount fallback, 完整新内容保留在 %s): %w", tmp, writeErr)
		}
		_ = os.Remove(tmp)
		if closeErr != nil {
			return nil, fmt.Errorf("replace config (bind mount fallback): %w", closeErr)
		}
	}

	// 4) 热应用：能立即生效的字段全部应用，并列出仍需重启的字段。
	live.Store(livecfg.Snapshot{
		APIKey:               newCfg.APIKey,
		SoftCooldown:         newCfg.SoftRateDur,
		SanitizeFingerprints: newCfg.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     newCfg.Logging.RequestClientInfo,
		HiddenModels:         livecfg.NormalizeHiddenModels(newCfg.Models.Hidden),
		BareModelNames:       newCfg.Models.BareNames,
		PreferredRealm:       newCfg.Models.PreferredRealm,
	})
	up.SanitizeFingerprints.Store(newCfg.Features.SanitizeBlacklistFingerprints)
	p.SetBreaker(newCfg.Pool.BreakerThreshold, newCfg.BreakerCooldownDur, newCfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(newCfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(newCfg.Pool.MaxInFlightGlobal)
	p.SetDegrade(newCfg.Pool.DegradeThreshold, newCfg.DegradeCooldownDur, newCfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(newCfg.SoftRateMaxDur)
	p.SetCostExploreInterval(newCfg.CostExploreIntervalDur) // costTier 探索窗口热生效（0 关停）
	p.SetCreditFloor(newCfg.Pool.CreditFloor)               // 积分保底热生效（0 = 关闭）
	p.SetDailyLimits(dailyLimitsOf(newCfg))                 // 每日护栏热生效
	p.SetWeights(newCfg.Pool.IdleWeightPerHour, newCfg.Pool.IdleWeightMax)
	p.SetPreferExpiring(newCfg.Pool.PreferExpiring)
	p.SetCrossDomainFallback(newCfg.Pool.CrossDomainFallback) // 裸名跨域回退热生效（issue #135）
	_ = p.SetScheduling(newCfg.Pool.Strategy, newCfg.Pool.AccountOrder)
	sch.SetExpiringSoonWindow(newCfg.ExpiringSoonDur)
	sch.Reconfigure(
		newCfg.Schedule.CheckinHours, newCfg.Schedule.TravelHours,
		newCfg.Schedule.ActivityHours, newCfg.Schedule.KeepaliveHours, newCfg.Schedule.BlackcatHours,
		newCfg.Schedule.GrowthHours,
		!newCfg.Schedule.CheckinEnabled, !newCfg.Schedule.TravelEnabled,
		!newCfg.Schedule.ActivityEnabled, !newCfg.Schedule.KeepaliveEnabled, !newCfg.Schedule.BlackcatEnabled,
		!newCfg.Schedule.GrowthEnabled)
	sch.SetBalanceInterval(newCfg.BalanceRefreshInterval)
	sch.SetTaskSweepInterval(newCfg.TaskSweepInterval)
	sch.SetIncludeDisabledInTasks(newCfg.Schedule.IncludeDisabledInTasks)

	return restartRequiredFields(newCfg), nil
}

// sqliteSettingsOf 从配置取 SQLite 存储设置。
func sqliteSettingsOf(c *Config) logstore.Settings {
	return logstore.Settings{
		Enabled:       c.Logging.SQLiteEnabled,
		RetentionDays: c.Logging.SQLiteRetentionDays,
		MaxRows:       c.Logging.SQLiteMaxRows,
		MaxMB:         c.Logging.SQLiteMaxMB,
	}
}

// applyCNChatBase 把 config cn.chat_base 装配进上游 client；空则保留内置默认，
// 老配置（无 cn 段）行为不变。
func applyCNChatBase(up *upstream.Client, cfg *Config) {
	if cfg.CN.ChatBase != "" {
		up.ChatBaseCN = cfg.CN.ChatBase
	}
}

// restartRequiredFields 返回本次改动中无法热生效、需要重启进程的字段名。
// 恒返回完整清单中的"与当前进程装配期依赖相关"的项——面板据此提示用户。
func restartRequiredFields(c *Config) []string {
	var out []string
	// 这些字段在进程内被监听地址/HTTP client/目录句柄等装配期对象捕获。
	if c.Listen != "" {
		out = append(out, "listen")
	}
	if c.AuthDir != "" {
		out = append(out, "auth_dir")
	}
	if c.StateFile != "" {
		out = append(out, "state_file")
	}
	out = append(out, "upstream.timeout_seconds", "upstream.header_timeout_seconds", "upstream.idle_timeout_seconds")
	// upstream.user_agent 在装配期被写进出站 client（main.go 的 up.UserAgent = ...），
	// 之后不再读取——不在 livecfg 热快照里，也无法热改。此前漏列，导致面板改完
	// 显示"已保存"却不提示需要重启，用户以为没生效（issue #102 附带发现 2）。
	out = append(out, "upstream.user_agent")
	// cn.chat_base 同样只在装配期写进 up.ChatBaseCN（请求路径并发读取，不做热改）。
	out = append(out, "cn.chat_base")
	if c.Upstash.URL != "" || c.Upstash.Token != "" {
		out = append(out, "upstash")
	}
	out = append(out, "session_sticky.ttl", "session_sticky.gc_interval")
	out = append(out, "logging.request_archive_enabled", "logging.request_retention_days", "logging.request_archive_max_mb")
	out = append(out, "server.read_timeout")
	return out
}

// mergeConfigMaps 把 incoming 深合并进 cur（原地），返回 cur。
// 对嵌套对象逐键覆盖而不是整体替换：面板表单只提交它管理的键，
// 未提交的兄弟键（含用户手写的未知键）保持原样。
func mergeConfigMaps(cur, incoming map[string]any) map[string]any {
	for k, v := range incoming {
		if inMap, ok := v.(map[string]any); ok {
			if curMap, ok := cur[k].(map[string]any); ok {
				cur[k] = mergeConfigMaps(curMap, inMap)
				continue
			}
		}
		cur[k] = v
	}
	return cur
}

// mergedJSON 把合并后的 map 序列化回 JSON（供 ParseConfig 校验）。
func mergedJSON(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// dailyLimitsOf 从配置取每日护栏阈值。
func dailyLimitsOf(c *Config) pool.DailyLimits {
	return pool.DailyLimits{
		Tokens:      c.Pool.DailyTokenLimit,
		Credits:     c.Pool.DailyCreditLimit,
		ModelTokens: c.Pool.ModelDailyTokenLimit,
	}
}
