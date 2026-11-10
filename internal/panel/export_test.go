package panel

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func newExportTestPanel(t *testing.T) (*Panel, *pool.Pool) {
	t.Helper()
	dir := t.TempDir()
	p := pool.New(filepath.Join(dir, "state.json"))
	t.Cleanup(p.Close)
	for i, uid := range []string{"uid-one", "uid-two", "uid-three"} {
		path := filepath.Join(dir, "workbuddy-"+uid+".json")
		raw := fmt.Sprintf(`{"auth":{"accessToken":"access-%s","refreshToken":"refresh-%s","expiresAt":1799999999,"domain":"","realm":"cn","custom":{"field":%d}},"account":{"uid":"%s","nickname":"账号 %d","enterpriseId":""},"device_token":"device-%s"}`, uid, uid, i, uid, i+1, uid)
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		a, err := auth.Parse([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		a.FilePath = path
		p.Add(a)
		p.SetCredits(uid, int64(100+i), int64(200+i))
	}
	p.Disable("uid-three", "manual test")
	return New(Config{Version: "test", APIKey: "test-key", Pool: p}), p
}

func requestAccountExport(t *testing.T, pn *Panel, method, path string, body io.Reader, authorized bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	if authorized {
		req.Header.Set("Authorization", "Bearer test-key")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	pn.ServeHTTP(recorder, req)
	return recorder
}

func decodeSingleExport(t *testing.T, recorder *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func TestAccountExportSinglePreservesCredentialsAndAddsStatus(t *testing.T) {
	pn, _ := newExportTestPanel(t)
	recorder := requestAccountExport(t, pn, http.MethodGet, "/panel/api/accounts/uid-one/export", nil, true)
	document := decodeSingleExport(t, recorder)
	var credential struct {
		Auth struct {
			AccessToken  string          `json:"accessToken"`
			RefreshToken string          `json:"refreshToken"`
			Custom       json.RawMessage `json:"custom"`
		} `json:"auth"`
		Account struct {
			UID      string `json:"uid"`
			Nickname string `json:"nickname"`
		} `json:"account"`
		DeviceToken string `json:"device_token"`
	}
	if err := json.Unmarshal(document["credential"], &credential); err != nil {
		t.Fatal(err)
	}
	if credential.Auth.AccessToken != "access-uid-one" || credential.Auth.RefreshToken != "refresh-uid-one" || credential.Account.UID != "uid-one" || credential.DeviceToken != "device-uid-one" {
		t.Fatalf("incomplete credential: %+v", credential)
	}
	if len(credential.Auth.Custom) == 0 {
		t.Fatal("unknown original credential fields must be retained")
	}
	var magpie struct {
		Type        string `json:"type"`
		Access      string `json:"access"`
		Refresh     string `json:"refresh"`
		Expires     int64  `json:"expires"`
		AccountID   string `json:"accountId"`
		UID         string `json:"uid"`
		Domain      string `json:"domain"`
		DeviceToken string `json:"device_token"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &magpie); err != nil {
		t.Fatal(err)
	}
	if magpie.Type != "oauth" || magpie.Access != "access-uid-one" || magpie.Refresh != "refresh-uid-one" || magpie.Expires != 1799999999000 || magpie.AccountID != "账号 1" || magpie.UID != "uid-one" || magpie.DeviceToken != "device-uid-one" {
		t.Fatalf("magpie-compatible fields incomplete: %+v", magpie)
	}
	var metadata struct {
		Schema   string `json:"schema"`
		Provider string `json:"provider"`
		Format   string `json:"format"`
		Source   struct {
			Name        string `json:"name"`
			Permissions string `json:"permissions"`
		} `json:"source_file"`
	}
	if err := json.Unmarshal(document["export_metadata"], &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Schema != "workbuddy-manage-panel/account-export/v2" || metadata.Provider != "workbuddy" || metadata.Format != "magpie/plugin-auth" || metadata.Source.Name != "workbuddy-uid-one.json" || metadata.Source.Permissions != "-rw-------" {
		t.Fatalf("metadata incomplete: %+v", metadata)
	}
	var status pool.Status
	if err := json.Unmarshal(document["pool_status"], &status); err != nil {
		t.Fatal(err)
	}
	if status.UID != "uid-one" || status.Credits != 100 || status.CreditsTotal != 200 {
		t.Fatalf("pool status incomplete: %+v", status)
	}
	_, params, err := mime.ParseMediaType(recorder.Header().Get("Content-Disposition"))
	if err != nil || params["filename"] != "workbuddy-账号_1-uid-one.json" || recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("X-Export-Contains-Credentials") != "true" {
		t.Fatalf("unsafe download headers: %v", recorder.Header())
	}
}

func TestAccountExportFrontendControls(t *testing.T) {
	index, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{`id="accSelectAll"`, `id="btnExportSelected"`, `id="accSelected"`} {
		if !strings.Contains(string(index), marker) {
			t.Fatalf("account selection control missing: %s", marker)
		}
	}
	script, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{`class="acc-export-check"`, `data-a="export"`, `colspan="10"`, `function downloadAccountExport`} {
		if !strings.Contains(string(script), marker) {
			t.Fatalf("account export frontend marker missing: %s", marker)
		}
	}
}

func TestAccountExportBatchZIP(t *testing.T) {
	pn, _ := newExportTestPanel(t)
	recorder := requestAccountExport(t, pn, http.MethodPost, "/panel/api/accounts/export", strings.NewReader(`{"uids":["uid-one","uid-three","uid-one"]}`), true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	archive, err := zip.NewReader(bytes.NewReader(recorder.Body.Bytes()), int64(recorder.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.File) != 2 || archive.File[0].Mode().Perm() != 0o600 || archive.File[1].Mode().Perm() != 0o600 {
		t.Fatalf("zip entries or permissions incorrect: %+v", archive.File)
	}
	seen := map[string]bool{}
	for _, entry := range archive.File {
		if !strings.HasPrefix(entry.Name, "accounts/workbuddy-") || !strings.HasSuffix(entry.Name, ".json") {
			t.Fatalf("unsafe or ambiguous filename: %q", entry.Name)
		}
		file, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		var document struct {
			PoolStatus pool.Status `json:"pool_status"`
		}
		if err := json.NewDecoder(file).Decode(&document); err != nil {
			file.Close()
			t.Fatal(err)
		}
		file.Close()
		seen[document.PoolStatus.UID] = true
		if document.PoolStatus.UID == "uid-three" && !document.PoolStatus.Disabled {
			t.Fatal("disabled runtime state missing")
		}
	}
	if !seen["uid-one"] || !seen["uid-three"] || seen["uid-two"] {
		t.Fatalf("wrong selected accounts: %v", seen)
	}
	if !strings.Contains(recorder.Header().Get("Content-Disposition"), ".zip") {
		t.Fatalf("expected zip download: %v", recorder.Header())
	}
}

func TestAccountExportBatchOneUIDUsesJSON(t *testing.T) {
	pn, _ := newExportTestPanel(t)
	recorder := requestAccountExport(t, pn, http.MethodPost, "/panel/api/accounts/export", strings.NewReader(`{"uids":["uid-two"]}`), true)
	document := decodeSingleExport(t, recorder)
	if len(document["credential"]) == 0 || len(document["pool_status"]) == 0 {
		t.Fatal("single account batch response is not a detailed JSON export")
	}
}

func TestAccountExportFlatGlobalEnterpriseCredentials(t *testing.T) {
	dir := t.TempDir()
	p := pool.New(filepath.Join(dir, "state.json"))
	t.Cleanup(p.Close)
	raw := []byte(`{"accessToken":"flat-access","refreshToken":"flat-refresh","expiresAt":1799999999000,"refreshExpiresAt":1899999999000,"domain":"www.workbuddy.ai","tokenType":"Bearer","realm":"global","uid":"flat-global","enterpriseId":"enterprise-1","nickname":"国际企业","custom":"retained"}`)
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	a.FilePath = filepath.Join(dir, "workbuddy-flat-global.json")
	if err := os.WriteFile(a.FilePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p.Add(a)
	pn := New(Config{APIKey: "test-key", Pool: p})
	recorder := requestAccountExport(t, pn, http.MethodGet, "/panel/api/accounts/flat-global/export", nil, true)
	document := decodeSingleExport(t, recorder)
	var exported struct {
		Access           string `json:"access"`
		Expires          int64  `json:"expires"`
		RefreshExpiresAt int64  `json:"refreshExpiresAt"`
		EnterpriseID     string `json:"enterpriseId"`
		Custom           string `json:"custom"`
		Metadata         struct {
			Provider string `json:"provider"`
		} `json:"export_metadata"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &exported); err != nil {
		t.Fatal(err)
	}
	if exported.Access != "flat-access" || exported.Expires != 1799999999000 || exported.RefreshExpiresAt != 1899999999000 || exported.EnterpriseID != "enterprise-1" || exported.Custom != "retained" || exported.Metadata.Provider != "workbuddy-ai" || len(document["credential"]) == 0 || len(document["pool_status"]) == 0 {
		t.Fatalf("flat global enterprise export incorrect: %+v", exported)
	}
}

func TestAccountExportValidationAndAuthorization(t *testing.T) {
	pn, _ := newExportTestPanel(t)
	for _, test := range []struct {
		method string
		path   string
		body   io.Reader
	}{
		{http.MethodGet, "/panel/api/accounts/uid-one/export", nil},
		{http.MethodPost, "/panel/api/accounts/export", strings.NewReader(`{"uids":["uid-one"]}`)},
	} {
		if recorder := requestAccountExport(t, pn, test.method, test.path, test.body, false); recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s without key: status=%d", test.path, recorder.Code)
		}
	}
	cases := []struct {
		body string
		want int
	}{
		{`{}`, http.StatusBadRequest},
		{`not-json`, http.StatusBadRequest},
		{`{"uids":["missing"]}`, http.StatusNotFound},
	}
	for _, test := range cases {
		recorder := requestAccountExport(t, pn, http.MethodPost, "/panel/api/accounts/export", strings.NewReader(test.body), true)
		if recorder.Code != test.want {
			t.Fatalf("body=%s: status=%d want=%d", test.body, recorder.Code, test.want)
		}
	}
	recorder := requestAccountExport(t, pn, http.MethodGet, "/panel/api/accounts/missing/export", nil, true)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("missing account: status=%d", recorder.Code)
	}
}
