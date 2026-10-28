package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDesktopInternalConfigAndHTTPAuthentication(t *testing.T) {
	dir := t.TempDir()
	i, err := Start(context.Background(), Options{
		ConfigPath: filepath.Join(dir, "config.json"), DataDir: dir,
		Listen: "127.0.0.1:0", RequireLoopback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer i.Close()
	for _, key := range []string{"first-key", "rotated-key"} {
		// The native asset handler can configure the first key and rotate it.
		rec := httptest.NewRecorder()
		i.DesktopHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/panel/api/config",
			strings.NewReader(`{"api_key":"`+key+`"}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("internal config save: status=%d body=%s", rec.Code, rec.Body.String())
		}
		for _, path := range []string{"/panel/api/config", "/status"} {
			for _, validKey := range []bool{false, true} {
				req, _ := http.NewRequest(http.MethodGet, strings.TrimSuffix(i.URL, "/panel/")+path, nil)
				req.Header.Set("User-Agent", "forged-client wails.io")
				want := http.StatusUnauthorized
				if validKey {
					req.Header.Set("Authorization", "Bearer "+key)
					want = http.StatusOK
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != want {
					t.Fatalf("HTTP %s (valid key=%v): status=%d want %d", path, validKey, resp.StatusCode, want)
				}
			}
		}
	}
}

func TestDesktopRelativePromptConfigLifecycle(t *testing.T) {
	for _, mode := range []string{"custom", "append"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "prompts"), 0700); err != nil {
				t.Fatal(err)
			}
			const text = "desktop prompt from data directory"
			if err := os.WriteFile(filepath.Join(dir, "prompts", "system.txt"), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "config.json")
			raw := []byte(`{"api_key":"original-key","prompt":{"mode":"` + mode + `","file":"prompts/system.txt"}}`)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			i, err := Start(context.Background(), Options{
				ConfigPath: path, DataDir: dir, Listen: "127.0.0.1:0", RequireLoopback: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer i.Close()
			if i.Config.APIKey != "original-key" || i.Config.PromptText != text || i.Config.Prompt.Mode != mode {
				t.Fatal("startup did not preserve configured key and prompt")
			}
			if i.Config.Prompt.File != filepath.Join(dir, "prompts", "system.txt") {
				t.Fatalf("runtime prompt path=%q", i.Config.Prompt.File)
			}
			rec := httptest.NewRecorder()
			i.DesktopHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/config", nil))
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"file":"prompts/system.txt"`) {
				t.Fatalf("config reload: status=%d body=%s", rec.Code, rec.Body.String())
			}
			rec = httptest.NewRecorder()
			i.DesktopHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/panel/api/config", strings.NewReader(`{"api_key":"updated-key"}`)))
			if rec.Code != http.StatusOK {
				t.Fatalf("save with relative prompt: status=%d body=%s", rec.Code, rec.Body.String())
			}
			saved, err := loadConfig(path, dir)
			if err != nil || saved.APIKey != "updated-key" || saved.Prompt.File != "prompts/system.txt" || saved.PromptText != text {
				t.Fatalf("saved config lost portable prompt or key: error=%v", err)
			}
		})
	}
}

func TestMissingReferencedPromptDoesNotFallBackToDefaults(t *testing.T) {
	for _, desktopMode := range []bool{false, true} {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		raw := []byte(`{"api_key":"keep-key","prompt":{"mode":"custom","file":"missing.txt"}}`)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		i, err := Start(context.Background(), Options{
			ConfigPath: path, DataDir: dir, Listen: "127.0.0.1:0", RequireLoopback: desktopMode,
		})
		if i != nil {
			i.Close()
			t.Fatal("started with defaults despite a missing referenced prompt")
		}
		if err == nil || !strings.Contains(err.Error(), "prompt file") {
			t.Fatalf("missing prompt: error=%v", err)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(raw, after) {
			t.Fatal("failed startup changed existing config")
		}
	}
}

func TestCloseCancelsModelWarmup(t *testing.T) {
	for _, realm := range []string{"cn", "global"} {
		t.Run(realm, func(t *testing.T) {
			started, cancelled := make(chan struct{}, 8), make(chan struct{}, 8)
			release := make(chan struct{})
			releaseAll := sync.OnceFunc(func() { close(release) })
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				started <- struct{}{}
				select {
				case <-r.Context().Done():
					cancelled <- struct{}{}
				case <-release:
				}
			}))
			t.Cleanup(up.Close)
			t.Cleanup(releaseAll)
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "auths"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "auths", "workbuddy-test.json"), []byte(
				`{"uid":"test-account","accessToken":"test-token","realm":"`+realm+`"}`), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := map[string]any{
				"upstream": map[string]any{"timeout_seconds": 30},
				"cn":       map[string]any{"chat_base": up.URL},
				"global":   map[string]any{"chat_base": up.URL},
				"schedule": map[string]any{
					"checkin_enabled": false, "travel_enabled": false, "activity_enabled": false,
					"keepalive_enabled": false, "blackcat_enabled": false, "growth_enabled": false,
					"balance_refresh_enabled": false, "task_sweep_enabled": false,
				},
			}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			i, err := Start(context.Background(), Options{
				ConfigPath: path, DataDir: dir, Listen: "127.0.0.1:0", RequireLoopback: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { releaseAll(); i.Close() })
			probes := 2
			if realm == "global" {
				probes = 4
			}
			for range probes {
				select {
				case <-started:
				case <-time.After(3 * time.Second):
					t.Fatal("warmup did not reach local upstream")
				}
			}
			done := make(chan error, 1)
			go func() { done <- i.Close() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Close waited for the 30-second upstream timeout")
			}
			for range probes {
				select {
				case <-cancelled:
				case <-time.After(3 * time.Second):
					t.Fatal("upstream request was not cancelled")
				}
			}
		})
	}
}
