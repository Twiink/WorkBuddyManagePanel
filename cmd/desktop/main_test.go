package main

import (
	"path/filepath"
	"testing"
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

func TestNewPanelTokenIsRandomAndNonEmpty(t *testing.T) {
	a, err := newPanelToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newPanelToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) < 32 || a == b {
		t.Fatalf("tokens must be non-empty and unique: len=%d equal=%v", len(a), a == b)
	}
}
