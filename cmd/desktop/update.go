package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/buildinfo"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/updater"
	"github.com/wailsapp/wails/v3/pkg/updater/providers/github"
)

const (
	updateRepository  = "Twiink/WorkBuddyManagePanel"
	updateReleasesURL = "https://github.com/" + updateRepository + "/releases/latest"
	updateAssetPrefix = "workbuddy-manage-panel"
	updateChecksums   = "checksums.txt"
	updateFirstCheck  = 30 * time.Second
	updateInterval    = 6 * time.Hour
)

// releaseVersion strips the "-panel" build suffix. Release tags carry the bare
// version, and semver ranks "1.15.1-panel" below "1.15.1", so passing the raw
// string would offer the running release as an update forever.
func releaseVersion(v string) string {
	return strings.TrimSuffix(v, "-panel")
}

// updateAssetSuffix names the release asset that can replace the running
// application in place. manual selects the .deb, which is only offered as a
// download link because dpkg owns /usr/bin. An empty result means no release
// asset fits this platform.
func updateAssetSuffix(goos, goarch string, manual bool) string {
	switch {
	case goos == "darwin":
		return "-macos-universal.zip"
	case goos == "windows" && goarch == "amd64":
		return "-windows-amd64-update.zip"
	case goos == "linux" && goarch == "amd64" && manual:
		return "_amd64.deb"
	case goos == "linux" && goarch == "amd64":
		return "-linux-amd64.AppImage"
	}
	return ""
}

// assetMatcher picks the desktop asset by exact naming convention. The
// default matcher only looks for platform and arch substrings and would pick
// the wb2api server archive for windows/amd64.
func assetMatcher(suffix string) github.AssetMatcher {
	return func(_ updater.CheckRequest, assets []github.ReleaseAsset) int {
		for i, a := range assets {
			if strings.HasPrefix(a.Name, updateAssetPrefix) && strings.HasSuffix(a.Name, suffix) {
				return i
			}
		}
		return -1
	}
}

// verifiedProvider refuses a release whose asset has no checksums.txt entry,
// so a missing sidecar cannot turn into an unverified install.
type verifiedProvider struct {
	updater.Provider
}

func (p verifiedProvider) Check(ctx context.Context, req updater.CheckRequest) (*updater.Release, error) {
	rel, err := p.Provider.Check(ctx, req)
	if err != nil || rel == nil {
		return rel, err
	}
	if rel.Verification == nil || len(rel.Verification.Digest) == 0 {
		return nil, fmt.Errorf("release %s has no %s entry for %s", rel.Version, updateChecksums, rel.Artifact.Filename)
	}
	return rel, nil
}

type desktopUpdater struct {
	app      *application.App
	updater  *updater.Updater
	manual   bool
	quitting *atomic.Bool
	busy     atomic.Bool

	promptedM sync.Mutex
	prompted  string
}

// newDesktopUpdater configures app.Updater against GitHub Releases. It
// returns nil when this platform has no updatable release asset.
func newDesktopUpdater(app *application.App, quitting *atomic.Bool) (*desktopUpdater, error) {
	// AppImage sets $APPIMAGE; any other Linux launch is the .deb install.
	manual := runtime.GOOS == "linux" && os.Getenv("APPIMAGE") == ""
	suffix := updateAssetSuffix(runtime.GOOS, runtime.GOARCH, manual)
	if suffix == "" {
		return nil, nil
	}
	gh, err := github.New(github.Config{
		Repository:    updateRepository,
		AssetMatcher:  assetMatcher(suffix),
		ChecksumAsset: updateChecksums,
	})
	if err != nil {
		return nil, err
	}
	if err := app.Updater.Init(updater.Config{
		CurrentVersion: releaseVersion(buildinfo.Version),
		Providers:      []updater.Provider{verifiedProvider{gh}},
		Window:         updater.WindowNone,
	}); err != nil {
		return nil, err
	}
	return &desktopUpdater{app: app, updater: app.Updater, manual: manual, quitting: quitting}, nil
}

// run checks shortly after launch and then periodically until ctx ends.
func (d *desktopUpdater) run(ctx context.Context) {
	timer := time.NewTimer(updateFirstCheck)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			d.check(ctx, false)
			timer.Reset(updateInterval)
		}
	}
}

// check looks for a newer release. Background checks stay silent unless they
// find a version not offered yet this session; manual checks always report.
func (d *desktopUpdater) check(ctx context.Context, manual bool) {
	if d.updater.State() == updater.StateReady {
		d.askRestart(ctx)
		return
	}
	if !d.busy.CompareAndSwap(false, true) {
		if manual {
			d.info("检查更新", "正在检查或下载更新，请稍候。")
		}
		return
	}
	rel, err := d.updater.Check(ctx)
	d.busy.Store(false)
	if err != nil {
		log.Printf("update check: %v", err)
		if manual {
			d.fail("检查更新失败", err)
		}
		return
	}
	if rel == nil {
		if manual {
			d.info("检查更新", fmt.Sprintf("当前已是最新版本 v%s。", releaseVersion(buildinfo.Version)))
		}
		return
	}
	log.Printf("update available: v%s (%s)", rel.Version, rel.Artifact.Filename)
	if !manual && !d.firstPrompt(rel.Version) {
		return
	}

	dialog := d.app.Dialog.Question().SetTitle("发现新版本")
	if d.manual {
		dialog.SetMessage(fmt.Sprintf("WorkBuddy Manage Panel v%s 已发布（当前 v%s）。\n\n.deb 安装需要下载新安装包手动更新，是否打开下载页面？",
			rel.Version, releaseVersion(buildinfo.Version)))
		open := dialog.AddButton("打开下载页面").OnClick(func() { d.openRelease(rel) })
		dialog.SetDefaultButton(open)
	} else {
		dialog.SetMessage(fmt.Sprintf("WorkBuddy Manage Panel v%s 已发布（当前 v%s）。\n\n是否立即下载并安装？下载完成后需要重启应用。",
			rel.Version, releaseVersion(buildinfo.Version)))
		install := dialog.AddButton("立即更新").OnClick(func() { go d.install(ctx) })
		dialog.SetDefaultButton(install)
	}
	dialog.SetCancelButton(dialog.AddButton("稍后"))
	dialog.Show()
}

func (d *desktopUpdater) firstPrompt(version string) bool {
	d.promptedM.Lock()
	defer d.promptedM.Unlock()
	if d.prompted == version {
		return false
	}
	d.prompted = version
	return true
}

func (d *desktopUpdater) install(ctx context.Context) {
	if !d.busy.CompareAndSwap(false, true) {
		return
	}
	err := d.updater.DownloadAndInstall(ctx)
	d.busy.Store(false)
	if err != nil {
		log.Printf("update download: %v", err)
		d.fail("更新下载失败", err)
		return
	}
	log.Printf("update staged at %s", d.updater.DownloadedPath())
	d.askRestart(ctx)
}

func (d *desktopUpdater) askRestart(ctx context.Context) {
	dialog := d.app.Dialog.Question().
		SetTitle("更新已就绪").
		SetMessage("新版本已下载并通过 SHA-256 校验。重启应用即可完成更新，网关会短暂中断。")
	restart := dialog.AddButton("立即重启").OnClick(func() { go d.restart(ctx) })
	dialog.SetDefaultButton(restart)
	dialog.SetCancelButton(dialog.AddButton("稍后"))
	dialog.Show()
}

func (d *desktopUpdater) restart(ctx context.Context) {
	// Restart quits the app; the hidden-window close hook must not veto it.
	d.quitting.Store(true)
	if err := d.updater.Restart(ctx); err != nil {
		d.quitting.Store(false)
		log.Printf("update restart: %v", err)
		d.fail("重启更新失败", err)
	}
}

func (d *desktopUpdater) openRelease(rel *updater.Release) {
	url, _ := rel.Metadata["github.release.htmlURL"].(string)
	if url == "" {
		url = updateReleasesURL
	}
	if err := d.app.Browser.OpenURL(url); err != nil {
		log.Printf("open release page: %v", err)
	}
}

func (d *desktopUpdater) info(title, message string) {
	d.app.Dialog.Info().SetTitle(title).SetMessage(message).Show()
}

func (d *desktopUpdater) fail(title string, err error) {
	d.app.Dialog.Error().SetTitle(title).SetMessage(err.Error()).Show()
}
