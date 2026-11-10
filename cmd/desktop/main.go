package main

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/linguo2625469/workbuddy2api-panel/internal/buildinfo"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed app-icon.png
var appIcon []byte

type desktop struct {
	app      *application.App
	gateway  *gateway.Instance
	window   *application.WebviewWindow
	panelURL string
	gatewayM sync.Mutex
	quitting atomic.Bool
}

func main() {
	if err := run(); err != nil {
		log.Printf("WorkBuddy Manage Panel: %v", err)
		fmt.Fprintln(os.Stderr, "启动失败。详细原因已写入终端和桌面日志。")
		os.Exit(1)
	}
}

func run() error {
	dataDir, err := desktopDataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create desktop data directory %s: %w", dataDir, err)
	}

	logPath := filepath.Join(dataDir, "desktop.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open desktop log %s: %w", logPath, err)
	}
	defer logFile.Close()
	log.SetOutput(io.MultiWriter(os.Stderr, logFile))
	log.Printf("%s %s starting; data=%s", buildinfo.DesktopName, buildinfo.Version, dataDir)

	d := &desktop{}
	assets := http.NewServeMux()
	// application.New acquires the single-instance lock synchronously. This
	// must happen before the gateway binds its fixed loopback port; otherwise a
	// second double-click fails on the port and never reaches the callback that
	// focuses the existing window.
	app := application.New(application.Options{
		Name:        buildinfo.DesktopName,
		Description: "WorkBuddy account pool and gateway",
		Icon:        appIcon,
		Assets: application.AssetOptions{
			Handler: assets,
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
		Windows: application.WindowsOptions{
			DisableQuitOnLastWindowClosed: true,
		},
		Linux: application.LinuxOptions{
			DisableQuitOnLastWindowClosed: true,
			ProgramName:                   buildinfo.BundleID,
			ApplicationID:                 buildinfo.BundleID,
		},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: BuildUniqueID,
			ExitCode: 0,
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				d.showWindow()
			},
		},
		OnShutdown: func() {
			d.gatewayM.Lock()
			instance := d.gateway
			d.gateway = nil
			d.gatewayM.Unlock()
			if instance != nil {
				if err := instance.Close(); err != nil {
					log.Printf("gateway shutdown: %v", err)
				}
			}
			log.Printf("%s stopped", buildinfo.DesktopName)
		},
	})
	d.app = app

	instance, err := gateway.Start(app.Context(), gateway.Options{
		ConfigPath:             filepath.Join(dataDir, "config.json"),
		DataDir:                dataDir,
		FallbackOnPortConflict: true,
		RequireLoopback:        true,
		Version:                buildinfo.Version,
	})
	if err != nil {
		return fmt.Errorf("start local gateway: %w", err)
	}
	d.gatewayM.Lock()
	d.gateway = instance
	d.gatewayM.Unlock()
	// 原生资源服务器在进程内路由管理请求，HTTP 网关单独验证 API Key。
	assets.Handle("/panel/", instance.DesktopHandler)
	panelHash := "accounts"
	if instance.Config.APIKey == "" {
		panelHash = "config"
	}
	d.panelURL = "/panel/#" + panelHash

	d.window = app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:            "main",
		Title:           desktopWindowTitle(instance),
		Width:           1360,
		Height:          900,
		MinWidth:        900,
		MinHeight:       600,
		InitialPosition: application.WindowCentered,
		URL:             d.panelURL,
		EnableFileDrop:  true,
		BackgroundColour: application.RGBA{
			Red: 12, Green: 14, Blue: 20, Alpha: 255,
		},
	})
	// 原生文件拖放（GTK/WebView2 拦截了 DOM 拖放事件，页面拿不到 File 对象）：
	// 读取拖入的 JSON 文件内容，经 __wbNativeImportFiles 桥送进面板导入队列。
	d.window.OnWindowEvent(events.Common.WindowFilesDropped, func(event *application.WindowEvent) {
		d.forwardDroppedFiles(event)
	})
	d.window.OnWindowEvent(events.Common.WindowFocus, func(*application.WindowEvent) {
		d.window.ExecJS("window.dispatchEvent(new Event('focus'));")
	})
	d.window.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
		if d.quitting.Load() {
			return
		}
		d.window.Hide()
		event.Cancel()
	})

	tray := app.SystemTray.New()
	tray.SetTooltip(desktopTrayTooltip(instance))
	tray.SetIcon(appIcon)

	updates, err := newDesktopUpdater(app, &d.quitting)
	if err != nil {
		log.Printf("updater disabled: %v", err)
	}

	menu := app.NewMenu()
	menu.Add("显示 WorkBuddy").OnClick(func(*application.Context) {
		d.showWindow()
	})
	menu.Add("在浏览器中打开面板").OnClick(func(*application.Context) {
		if err := app.Browser.OpenURL(instance.URL); err != nil {
			log.Printf("open panel in browser: %v", err)
		}
	})
	if updates != nil {
		menu.Add("检查更新…").OnClick(func(*application.Context) {
			go updates.check(app.Context(), true)
		})
		go updates.run(app.Context())
	}
	menu.AddSeparator()
	menu.Add("退出").OnClick(func(*application.Context) {
		d.quitting.Store(true)
		app.Quit()
	})
	tray.SetMenu(menu)
	tray.OnClick(func() {
		d.showWindow()
	})

	if runtime.GOOS == "darwin" {
		app.Event.OnApplicationEvent(events.Mac.ApplicationShouldHandleReopen, func(*application.ApplicationEvent) {
			d.showWindow()
		})
	}

	log.Printf("panel ready at %s", instance.URL)
	if err := app.Run(); err != nil {
		return fmt.Errorf("run desktop application: %w", err)
	}
	return nil
}

func (d *desktop) showWindow() {
	if d.window == nil {
		return
	}
	d.window.Show()
	d.window.Restore()
	d.window.UnMinimise()
	d.window.Focus()
	d.window.ExecJS("window.dispatchEvent(new Event('focus'));")
}

// forwardDroppedFiles 把拖进窗口的 JSON 文件转发给面板导入队列。
// 文件内容走 base64，避免在 JS 里读取本地路径；非 JSON 与超大文件记日志跳过。
func (d *desktop) forwardDroppedFiles(event *application.WindowEvent) {
	paths := event.Context().DroppedFiles()
	if len(paths) == 0 || d.window == nil {
		return
	}
	type droppedFile struct {
		Name string `json:"name"`
		Data string `json:"data"`
	}
	payload := make([]droppedFile, 0, len(paths))
	skipped := 0
	for _, path := range paths {
		if !strings.EqualFold(filepath.Ext(path), ".json") {
			skipped++
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			log.Printf("panel: 读取拖入文件失败 %s: %v", filepath.Base(path), err)
			continue
		}
		if len(content) > 12<<20 {
			log.Printf("panel: 拖入文件过大已跳过 %s (%d bytes)", filepath.Base(path), len(content))
			continue
		}
		payload = append(payload, droppedFile{
			Name: filepath.Base(path),
			Data: base64.StdEncoding.EncodeToString(content),
		})
	}
	if skipped > 0 {
		d.window.ExecJS("window.toast && window.toast('已忽略 " + strconv.Itoa(skipped) + " 个非 JSON 文件', 'err');")
	}
	if len(payload) == 0 {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		log.Printf("panel: 序列化拖入文件失败: %v", err)
		return
	}
	d.window.ExecJS("window.__wbNativeImportFiles && window.__wbNativeImportFiles(" + string(raw) + ");")
}

func desktopDataDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(base, "WorkBuddyManagePanel"), nil
}

func desktopWindowTitle(instance *gateway.Instance) string {
	if instance == nil || instance.ListenPort == 0 {
		return buildinfo.DesktopName
	}
	if instance.PortFallback {
		return fmt.Sprintf("%s · 端口 %d（%s 被占用）", buildinfo.DesktopName, instance.ListenPort, instance.ConfiguredListen)
	}
	return fmt.Sprintf("%s · 端口 %d", buildinfo.DesktopName, instance.ListenPort)
}

func desktopTrayTooltip(instance *gateway.Instance) string {
	if instance == nil || instance.ListenPort == 0 {
		return buildinfo.DesktopName
	}
	if instance.PortFallback {
		return fmt.Sprintf("%s · 端口 %d（已自动切换）", buildinfo.DesktopName, instance.ListenPort)
	}
	return fmt.Sprintf("%s · 端口 %d", buildinfo.DesktopName, instance.ListenPort)
}

// BuildUniqueID is stable across releases so a second launch focuses the
// existing process rather than attempting to bind the gateway port again.
const BuildUniqueID = "io.github.twiink.workbuddy-manage-panel"
