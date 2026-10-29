package main

import (
	"context"
	"crypto/sha256"
	"io"
	"os"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/buildinfo"
	"github.com/wailsapp/wails/v3/pkg/updater"
	"github.com/wailsapp/wails/v3/pkg/updater/providers/github"
)

// Asset names published by .github/workflows/go-binaries.yml for v1.15.1,
// plus the Windows update archive.
var releaseAssets = []github.ReleaseAsset{
	{Name: "checksums.txt"},
	{Name: "wb2api-panel-1.15.1-darwin-amd64.tar.gz"},
	{Name: "wb2api-panel-1.15.1-linux-amd64.tar.gz"},
	{Name: "wb2api-panel-1.15.1-windows-amd64.zip"},
	{Name: "workbuddy-manage-panel-1.15.1-linux-amd64.AppImage"},
	{Name: "workbuddy-manage-panel-1.15.1-macos-universal.dmg"},
	{Name: "workbuddy-manage-panel-1.15.1-macos-universal.zip"},
	{Name: "workbuddy-manage-panel-1.15.1-windows-amd64-setup.exe"},
	{Name: "workbuddy-manage-panel-1.15.1-windows-amd64-update.zip"},
	{Name: "workbuddy-manage-panel_1.15.1_amd64.deb"},
}

func TestUpdateAssetSelection(t *testing.T) {
	cases := []struct {
		goos, goarch string
		debInstall   bool
		want         string
	}{
		{"darwin", "arm64", false, "workbuddy-manage-panel-1.15.1-macos-universal.zip"},
		{"darwin", "amd64", false, "workbuddy-manage-panel-1.15.1-macos-universal.zip"},
		{"windows", "amd64", false, "workbuddy-manage-panel-1.15.1-windows-amd64-update.zip"},
		{"linux", "amd64", false, "workbuddy-manage-panel-1.15.1-linux-amd64.AppImage"},
		{"linux", "amd64", true, "workbuddy-manage-panel_1.15.1_amd64.deb"},
	}
	for _, tc := range cases {
		suffix := updateAssetSuffix(tc.goos, tc.goarch, tc.debInstall)
		idx := assetMatcher(suffix)(updater.CheckRequest{}, releaseAssets)
		if idx < 0 || releaseAssets[idx].Name != tc.want {
			t.Errorf("%s/%s debInstall=%v picked %d, want %s", tc.goos, tc.goarch, tc.debInstall, idx, tc.want)
		}
	}
	if s := updateAssetSuffix("linux", "arm64", false); s != "" {
		t.Errorf("linux/arm64 has no desktop asset, got suffix %q", s)
	}
}

func TestUpdateMatcherMissingAsset(t *testing.T) {
	old := []github.ReleaseAsset{{Name: "workbuddy-manage-panel-1.15.1-windows-amd64-setup.exe"}}
	if idx := assetMatcher(updateAssetSuffix("windows", "amd64", false))(updater.CheckRequest{}, old); idx != -1 {
		t.Fatalf("setup.exe must not be used as an in-place update, got %d", idx)
	}
}

func TestReleaseVersion(t *testing.T) {
	if got := releaseVersion("1.15.1-panel"); got != "1.15.1" {
		t.Fatalf("releaseVersion = %q", got)
	}
	if got := releaseVersion(buildinfo.Version); got == buildinfo.Version {
		t.Fatalf("buildinfo.Version %q no longer ends in -panel; update releaseVersion", buildinfo.Version)
	}
}

type stubProvider struct{ rel *updater.Release }

func (stubProvider) Name() string { return "stub" }
func (p stubProvider) Check(context.Context, updater.CheckRequest) (*updater.Release, error) {
	return p.rel, nil
}
func (stubProvider) Download(context.Context, *updater.Release, io.Writer, func(int64, int64)) error {
	return nil
}

func TestVerifiedProviderRequiresDigest(t *testing.T) {
	unverified := &updater.Release{Version: "9.0.0", Artifact: updater.Artifact{Filename: "x.zip"}}
	if _, err := (verifiedProvider{stubProvider{unverified}}).Check(context.Background(), updater.CheckRequest{}); err == nil {
		t.Fatal("release without checksum must be rejected")
	}
	verified := &updater.Release{Version: "9.0.0", Verification: &updater.Verification{DigestAlgo: "sha256", Digest: []byte{1}}}
	rel, err := (verifiedProvider{stubProvider{verified}}).Check(context.Background(), updater.CheckRequest{})
	if err != nil || rel != verified {
		t.Fatalf("verified release: rel=%v err=%v", rel, err)
	}
	if rel, err := (verifiedProvider{stubProvider{nil}}).Check(context.Background(), updater.CheckRequest{}); rel != nil || err != nil {
		t.Fatalf("up to date: rel=%v err=%v", rel, err)
	}
}

type downloadProvider struct {
	body []byte
}

func (downloadProvider) Name() string { return "download-test" }
func (downloadProvider) Check(context.Context, updater.CheckRequest) (*updater.Release, error) {
	return nil, nil
}
func (p downloadProvider) Download(_ context.Context, _ *updater.Release, dst io.Writer, progress func(int64, int64)) error {
	if _, err := dst.Write(p.body); err != nil {
		return err
	}
	if progress != nil {
		progress(int64(len(p.body)), int64(len(p.body)))
	}
	return nil
}

func TestDownloadDebVerifiesArtifact(t *testing.T) {
	body := []byte("a verified deb payload")
	rel := &updater.Release{
		Artifact: updater.Artifact{
			Filename: "workbuddy-manage-panel_9.9.9_amd64.deb",
			Size:     int64(len(body)),
		},
	}
	path, cleanup, err := downloadDeb(context.Background(), downloadProvider{body: body}, rel)
	if err != nil {
		t.Fatalf("downloadDeb: %v", err)
	}
	defer cleanup()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read downloaded payload: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("downloaded payload = %q, want %q", got, body)
	}
	digest := sha256.Sum256(body)
	if err := verifySHA256(path, &updater.Verification{DigestAlgo: "sha256", Digest: digest[:]}); err != nil {
		t.Fatalf("verifySHA256: %v", err)
	}
	digest[0] ^= 0xff
	if err := verifySHA256(path, &updater.Verification{DigestAlgo: "sha256", Digest: digest[:]}); err == nil {
		t.Fatal("verifySHA256 accepted a mismatched digest")
	}
}

func TestDownloadDebRejectsNonDebArtifact(t *testing.T) {
	rel := &updater.Release{Artifact: updater.Artifact{Filename: "workbuddy-manage-panel.zip"}}
	if _, _, err := downloadDeb(context.Background(), downloadProvider{}, rel); err == nil {
		t.Fatal("downloadDeb accepted a non-deb artifact")
	}
}
