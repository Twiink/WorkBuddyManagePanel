package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
	updateRepository = "Twiink/WorkBuddyManagePanel"
	updateChecksums  = "checksums.txt"
	updateFirstCheck = 30 * time.Second
	updateInterval   = 6 * time.Hour

	debPackageName    = "workbuddy-manage-panel"
	updateAssetPrefix = debPackageName
	pkexecPath        = "/usr/bin/pkexec"
	aptGetPath        = "/usr/bin/apt-get"
	envPath           = "/usr/bin/env"
	dpkgQueryPath     = "/usr/bin/dpkg-query"
	dpkgDebPath       = "/usr/bin/dpkg-deb"
	debInstallTimeout = 15 * time.Minute
)

// releaseVersion strips the "-panel" build suffix. Release tags carry the bare
// version, and semver ranks "1.15.1-panel" below "1.15.1", so passing the raw
// string would offer the running release as an update forever.
func releaseVersion(v string) string {
	return strings.TrimSuffix(v, "-panel")
}

// updateAssetSuffix names the release asset that can replace the running
// application. debInstall selects the .deb, while AppImage installs keep
// using the portable AppImage asset. An empty result means no release asset
// fits this platform.
func updateAssetSuffix(goos, goarch string, debInstall bool) string {
	switch {
	case goos == "darwin":
		return "-macos-universal.zip"
	case goos == "windows" && goarch == "amd64":
		return "-windows-amd64-update.zip"
	case goos == "linux" && goarch == "amd64" && debInstall:
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
	app        *application.App
	updater    *updater.Updater
	provider   updater.Provider
	debInstall bool
	quitting   *atomic.Bool
	busy       atomic.Bool

	promptedM sync.Mutex
	prompted  string
}

// newDesktopUpdater configures app.Updater against GitHub Releases. It
// returns nil when this platform has no updatable release asset.
func newDesktopUpdater(app *application.App, quitting *atomic.Bool) (*desktopUpdater, error) {
	// AppImage sets $APPIMAGE. Only a package-owned Linux binary is treated as
	// a .deb install; a source-tree build must not invoke the system package
	// manager for itself.
	debInstall := isDebInstall()
	if runtime.GOOS == "linux" && os.Getenv("APPIMAGE") == "" && !debInstall {
		return nil, nil
	}
	suffix := updateAssetSuffix(runtime.GOOS, runtime.GOARCH, debInstall)
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
	return &desktopUpdater{
		app:        app,
		updater:    app.Updater,
		provider:   gh,
		debInstall: debInstall,
		quitting:   quitting,
	}, nil
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
	if d.debInstall {
		dialog.SetMessage(fmt.Sprintf("WorkBuddy Manage Panel v%s 已发布（当前 v%s）。\n\n将下载 .deb 并请求系统管理员授权安装，是否立即更新？",
			rel.Version, releaseVersion(buildinfo.Version)))
		install := dialog.AddButton("立即更新").OnClick(func() { go d.install(ctx, rel) })
		dialog.SetDefaultButton(install)
	} else {
		dialog.SetMessage(fmt.Sprintf("WorkBuddy Manage Panel v%s 已发布（当前 v%s）。\n\n是否立即下载并安装？下载完成后需要重启应用。",
			rel.Version, releaseVersion(buildinfo.Version)))
		install := dialog.AddButton("立即更新").OnClick(func() { go d.install(ctx, rel) })
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

func (d *desktopUpdater) install(ctx context.Context, rel *updater.Release) {
	if !d.busy.CompareAndSwap(false, true) {
		return
	}
	var err error
	if d.debInstall {
		// apt owns the installed binary; let the package manager replace it
		// instead of using the single-file updater.
		err = d.installDeb(ctx, rel)
	} else {
		err = d.updater.DownloadAndInstall(ctx)
	}
	d.busy.Store(false)
	if err != nil {
		log.Printf("update install: %v", err)
		d.fail("更新失败", err)
		return
	}
	if d.debInstall {
		d.offerDebRestart(rel)
		return
	}
	log.Printf("update staged at %s", d.updater.DownloadedPath())
	d.askRestart(ctx)
}

func (d *desktopUpdater) installDeb(ctx context.Context, rel *updater.Release) error {
	if rel == nil {
		return fmt.Errorf("deb update: missing release")
	}
	if d.provider == nil {
		return fmt.Errorf("deb update: update provider unavailable")
	}

	debPath, cleanup, err := downloadDeb(ctx, d.provider, rel)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := verifySHA256(debPath, rel.Verification); err != nil {
		return err
	}
	if err := validateDebPackage(ctx, debPath); err != nil {
		return err
	}
	if err := installDebPackage(debPath); err != nil {
		return err
	}
	log.Printf("deb update installed: v%s", rel.Version)
	return nil
}

func downloadDeb(ctx context.Context, provider updater.Provider, rel *updater.Release) (string, func(), error) {
	name := filepath.Base(rel.Artifact.Filename)
	if name == "." || name == "" || !strings.HasSuffix(strings.ToLower(name), ".deb") {
		return "", nil, fmt.Errorf("deb update: unexpected artifact %q", rel.Artifact.Filename)
	}
	dir, err := os.MkdirTemp("", "workbuddy-deb-update-")
	if err != nil {
		return "", nil, fmt.Errorf("deb update: create temporary directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("deb update: create temporary file: %w", err)
	}

	success := false
	defer func() {
		if !success {
			_ = f.Close()
			cleanup()
		}
	}()

	lastProgress := int64(0)
	onProgress := func(written, total int64) {
		if total <= 0 {
			return
		}
		if written == total || written-lastProgress >= total/10 {
			lastProgress = written
			log.Printf("deb update download: %d%%", written*100/total)
		}
	}
	if err := provider.Download(ctx, rel, f, onProgress); err != nil {
		return "", nil, fmt.Errorf("deb update: download: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", nil, fmt.Errorf("deb update: sync: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", nil, fmt.Errorf("deb update: close: %w", err)
	}
	if rel.Artifact.Size > 0 {
		info, err := os.Stat(path)
		if err != nil {
			return "", nil, fmt.Errorf("deb update: stat: %w", err)
		}
		if info.Size() != rel.Artifact.Size {
			return "", nil, fmt.Errorf("deb update: size mismatch: got %d, want %d", info.Size(), rel.Artifact.Size)
		}
	}
	success = true
	return path, cleanup, nil
}

func verifySHA256(path string, verification *updater.Verification) error {
	if verification == nil || len(verification.Digest) == 0 {
		return fmt.Errorf("deb update: checksum missing")
	}
	if verification.DigestAlgo != "" && !strings.EqualFold(verification.DigestAlgo, "sha256") {
		return fmt.Errorf("deb update: unsupported checksum algorithm %q", verification.DigestAlgo)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("deb update: open for checksum: %w", err)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		_ = f.Close()
		return fmt.Errorf("deb update: checksum: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("deb update: close after checksum: %w", err)
	}
	if !bytes.Equal(hasher.Sum(nil), verification.Digest) {
		return fmt.Errorf("deb update: SHA-256 checksum mismatch")
	}
	return nil
}

func validateDebPackage(ctx context.Context, path string) error {
	if _, err := os.Stat(dpkgDebPath); err != nil {
		return fmt.Errorf("deb update: dpkg-deb unavailable: %w", err)
	}
	output, err := exec.CommandContext(ctx, dpkgDebPath, "-f", path, "Package").Output()
	if err != nil {
		return fmt.Errorf("deb update: validate package: %w", err)
	}
	if got := strings.TrimSpace(string(output)); got != debPackageName {
		return fmt.Errorf("deb update: unexpected package %q", got)
	}
	return nil
}

func installDebPackage(path string) error {
	for _, executable := range []string{pkexecPath, aptGetPath, envPath} {
		if _, err := os.Stat(executable); err != nil {
			return fmt.Errorf("deb update: required executable %s unavailable: %w", executable, err)
		}
	}
	installCtx, cancel := context.WithTimeout(context.Background(), debInstallTimeout)
	defer cancel()
	cmd := exec.CommandContext(
		installCtx,
		pkexecPath,
		envPath,
		"DEBIAN_FRONTEND=noninteractive",
		aptGetPath,
		"install",
		"-y",
		"--",
		path,
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(output))
	if len(detail) > 4000 {
		detail = detail[len(detail)-4000:]
	}
	if detail == "" {
		return fmt.Errorf("deb update: system package install failed: %w", err)
	}
	return fmt.Errorf("deb update: system package install failed: %w\n%s", err, detail)
}

func isDebInstall() bool {
	if runtime.GOOS != "linux" || os.Getenv("APPIMAGE") != "" {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	if _, err := os.Stat(dpkgQueryPath); err != nil {
		return false
	}
	output, err := exec.Command(dpkgQueryPath, "-S", exe).Output()
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(string(output)), debPackageName+":")
}

func (d *desktopUpdater) offerDebRestart(rel *updater.Release) {
	version := ""
	if rel != nil {
		version = rel.Version
	}
	dialog := d.app.Dialog.Question().
		SetTitle("更新已安装").
		SetMessage(fmt.Sprintf("WorkBuddy Manage Panel v%s 已安装。重启应用后生效。", version))
	restart := dialog.AddButton("立即重启").OnClick(func() { go d.restartInstalled() })
	dialog.SetDefaultButton(restart)
	dialog.SetCancelButton(dialog.AddButton("稍后"))
	dialog.Show()
}

func (d *desktopUpdater) restartInstalled() {
	exe, err := os.Executable()
	if err != nil {
		d.fail("重启失败", fmt.Errorf("定位当前程序: %w", err))
		return
	}
	shellArgs := []string{
		"-c",
		`while kill -0 "$1" 2>/dev/null; do sleep 0.1; done; shift; exec "$0" "$@"`,
		exe,
		strconv.Itoa(os.Getpid()),
	}
	shellArgs = append(shellArgs, os.Args[1:]...)
	cmd := exec.Command("/bin/sh", shellArgs...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		d.fail("重启失败", err)
		return
	}
	d.quitting.Store(true)
	d.app.Quit()
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

func (d *desktopUpdater) info(title, message string) {
	d.app.Dialog.Info().SetTitle(title).SetMessage(message).Show()
}

func (d *desktopUpdater) fail(title string, err error) {
	d.app.Dialog.Error().SetTitle(title).SetMessage(err.Error()).Show()
}
