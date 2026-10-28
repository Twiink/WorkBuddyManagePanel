package gateway

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveConfigPathsAnchorsDesktopFiles(t *testing.T) {
	base := t.TempDir()
	absolute := filepath.Join(t.TempDir(), "absolute-state.json")
	cfg := Default()
	cfg.AuthDir = "./auths"
	cfg.StateFile = "data/state.json"
	cfg.Prompt.File = "prompts/system.md"
	cfg.Upstream.DeviceTokenFile = "device-token"

	resolveConfigPaths(cfg, base)

	tests := map[string]string{
		"auth_dir":          filepath.Join(base, "auths"),
		"state_file":        filepath.Join(base, "data", "state.json"),
		"prompt.file":       filepath.Join(base, "prompts", "system.md"),
		"device_token_file": filepath.Join(base, "device-token"),
	}
	got := map[string]string{
		"auth_dir":          cfg.AuthDir,
		"state_file":        cfg.StateFile,
		"prompt.file":       cfg.Prompt.File,
		"device_token_file": cfg.Upstream.DeviceTokenFile,
	}
	for name, want := range tests {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}

	cfg.StateFile = absolute
	resolveConfigPaths(cfg, base)
	if cfg.StateFile != absolute {
		t.Fatalf("absolute state path changed to %q", cfg.StateFile)
	}
}

func TestRequireLoopbackListen(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:7863", "[::1]:7863", "localhost:7863"} {
		if err := requireLoopbackListen(listen); err != nil {
			t.Errorf("requireLoopbackListen(%q) = %v", listen, err)
		}
	}
	for _, listen := range []string{"0.0.0.0:7863", ":7863", "192.168.1.2:7863", "[::]:7863"} {
		if err := requireLoopbackListen(listen); err == nil {
			t.Errorf("requireLoopbackListen(%q) accepted a non-loopback address", listen)
		}
	}
}

func TestNormalizeLoopbackListen(t *testing.T) {
	got, err := normalizeLoopbackListen(":7864")
	if err != nil {
		t.Fatal(err)
	}
	if got != "127.0.0.1:7864" {
		t.Fatalf("normalizeLoopbackListen(:7864) = %q", got)
	}
	for _, listen := range []string{"127.0.0.1:7864", "localhost:7864", "[::1]:7864"} {
		got, err := normalizeLoopbackListen(listen)
		if err != nil {
			t.Errorf("normalizeLoopbackListen(%q) = %v", listen, err)
		}
		if got != listen {
			t.Errorf("normalizeLoopbackListen(%q) = %q", listen, got)
		}
	}
	for _, listen := range []string{"7864", "127.0.0.1", ":not-a-port", "0.0.0.0:7864"} {
		if got, err := normalizeLoopbackListen(listen); err == nil {
			t.Errorf("normalizeLoopbackListen(%q) = %q, want error", listen, got)
		}
	}
}

func TestStartServesPanelAndClosesIdempotently(t *testing.T) {
	dataDir := t.TempDir()
	instance, err := Start(context.Background(), Options{
		ConfigPath:      filepath.Join(dataDir, "config.json"),
		DataDir:         dataDir,
		Listen:          "127.0.0.1:0",
		RequireLoopback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	address := instance.URL
	resp, err := http.Get(address)
	if err != nil {
		_ = instance.Close()
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = instance.Close()
		t.Fatalf("GET %s = %d", address, resp.StatusCode)
	}

	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	if err := instance.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}

	listen := strings.TrimSuffix(instance.URL, instance.URLPath)
	listen = strings.TrimPrefix(listen, "http://")
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		t.Fatalf("port not released after Close: %v", err)
	}
	_ = listener.Close()
}

func TestStartRejectsNonLoopbackInDesktopMode(t *testing.T) {
	dataDir := t.TempDir()
	instance, err := Start(context.Background(), Options{
		ConfigPath:      filepath.Join(dataDir, "config.json"),
		DataDir:         dataDir,
		Listen:          "0.0.0.0:0",
		RequireLoopback: true,
	})
	if instance != nil {
		_ = instance.Close()
		t.Fatal("desktop mode started a non-loopback listener")
	}
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("Start() error = %v, want loopback rejection", err)
	}
}

func TestStartNormalizesWildcardPortForDesktop(t *testing.T) {
	dataDir := t.TempDir()
	instance, err := Start(context.Background(), Options{
		ConfigPath:      filepath.Join(dataDir, "config.json"),
		DataDir:         dataDir,
		Listen:          ":0",
		RequireLoopback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	if !strings.HasPrefix(instance.URL, "http://127.0.0.1:") {
		t.Fatalf("desktop URL = %q, want loopback", instance.URL)
	}
}

func TestDesktopFirstRunCreatesEmptyKeyAndServerFirstRunCreatesRandomKey(t *testing.T) {
	desktopDir := t.TempDir()
	desktop, err := Start(context.Background(), Options{
		ConfigPath:      filepath.Join(desktopDir, "config.json"),
		DataDir:         desktopDir,
		Listen:          "127.0.0.1:0",
		RequireLoopback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if desktop.Config.APIKey != "" {
		_ = desktop.Close()
		t.Fatalf("desktop first run api_key=%q, want empty user-configurable key", desktop.Config.APIKey)
	}
	if err := desktop.Close(); err != nil {
		t.Fatal(err)
	}

	serverDir := t.TempDir()
	server, err := Start(context.Background(), Options{
		ConfigPath: filepath.Join(serverDir, "config.json"),
		DataDir:    serverDir,
		Listen:     "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if !strings.HasPrefix(server.Config.APIKey, "sk-") {
		t.Fatalf("server first run api_key=%q, want generated sk- key", server.Config.APIKey)
	}
}

func TestStartFailsCleanlyWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dataDir := t.TempDir()
	instance, err := Start(ctx, Options{
		ConfigPath: filepath.Join(dataDir, "config.json"),
		DataDir:    dataDir,
		Listen:     "127.0.0.1:0",
	})
	if err == nil {
		_ = instance.Close()
		t.Fatal("Start() succeeded with an already-cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "state.json")); statErr == nil {
		t.Fatal("failed startup left a state file behind")
	}
}

func TestStartFailureReleasesPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	dataDir := t.TempDir()
	_, err = Start(context.Background(), Options{
		ConfigPath:      filepath.Join(dataDir, "config.json"),
		DataDir:         dataDir,
		Listen:          listener.Addr().String(),
		RequireLoopback: true,
	})
	if err == nil {
		t.Fatal("Start() unexpectedly succeeded on an occupied port")
	}
}

func TestStartWaitsForCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dataDir := t.TempDir()
	instance, err := Start(ctx, Options{
		ConfigPath: filepath.Join(dataDir, "config.json"),
		DataDir:    dataDir,
		Listen:     "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel()

	deadline := time.Now().Add(5 * time.Second)
	for ctx.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
}
