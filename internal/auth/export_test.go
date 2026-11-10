package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestExportRawPreservesCompleteCredentialDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workbuddy-u1.json")
	original := []byte(`{
  "auth": {"accessToken": "at", "refreshToken": "rt", "expiresAt": 1, "domain": "", "realm": "cn", "custom": "keep"},
  "account": {"uid": "u1", "nickname": "账号", "custom": {"detail": true}},
  "device_token": "device",
  "proxy": "socks5://127.0.0.1:1080"
}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Auth{UID: "u1", FilePath: path}
	raw, err := a.ExportRaw()
	if err != nil {
		t.Fatal(err)
	}
	var exported, expected any
	if err := json.Unmarshal(raw, &exported); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(original, &expected); err != nil {
		t.Fatal(err)
	}
	left, _ := json.Marshal(exported)
	right, _ := json.Marshal(expected)
	if string(left) != string(right) {
		t.Fatalf("credential document changed\nwant: %s\n got: %s", right, left)
	}
}

func TestExportRawRejectsMissingOrInvalidFile(t *testing.T) {
	a := &Auth{UID: "u1"}
	if _, err := a.ExportRaw(); err == nil {
		t.Fatal("missing FilePath must fail")
	}
	a.FilePath = filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(a.FilePath, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ExportRaw(); err == nil {
		t.Fatal("invalid JSON must fail")
	}
}
