package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
)

func TestDesktopDataDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	got, err := desktopDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "WorkBuddyManagePanel" {
		t.Fatalf("desktopDataDir() = %q", got)
	}
}

func TestBuildIdentityStable(t *testing.T) {
	const want = "io.github.twiink.workbuddy-manage-panel"
	if BuildUniqueID != want {
		t.Fatalf("BuildUniqueID = %q, want %q", BuildUniqueID, want)
	}
}

func TestDesktopWindowTitleShowsActualPort(t *testing.T) {
	got := desktopWindowTitle(&gateway.Instance{ListenPort: 7863})
	if !strings.Contains(got, "端口 7863") {
		t.Fatalf("desktopWindowTitle() = %q, want actual port", got)
	}
	got = desktopWindowTitle(&gateway.Instance{
		ConfiguredListen: "127.0.0.1:7863",
		ListenPort:       41234,
		PortFallback:     true,
	})
	if !strings.Contains(got, "端口 41234") || !strings.Contains(got, "7863 被占用") {
		t.Fatalf("desktopWindowTitle(fallback) = %q, want actual port and fallback notice", got)
	}
}
