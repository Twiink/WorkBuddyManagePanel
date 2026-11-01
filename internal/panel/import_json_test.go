package panel

// cockpit 导入的 JSON+base64 传输路径（桌面端 WebView 对 multipart FormData
// 支持不稳，前端改发 JSON；后端同时兼容 multipart，见 import_cockpit_test）。

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func newImportTestPanel(t *testing.T) (*Panel, string) {
	t.Helper()
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := pool.New(filepath.Join(dir, "state.json"))
	t.Cleanup(p.Close)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"msg":"ok","data":{}}`)
	}))
	t.Cleanup(stub.Close)
	pn := New(Config{
		Version:  "test",
		APIKey:   "test-key",
		Pool:     p,
		Upstream: &upstream.Client{HTTP: stub.Client(), ChatBaseCN: stub.URL, BillingBaseCN: stub.URL},
		AuthDir:  authDir,
	})
	return pn, authDir
}

func postImportJSON(t *testing.T, pn *Panel, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	pn.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

func TestImportCockpitJSONBody(t *testing.T) {
	pn, authDir := newImportTestPanel(t)
	accounts := `[
		{"id":"a1","uid":"uid-json-1","email":"a1@x","nickname":"甲","access_token":"at","refresh_token":"rt","expires_at":1795874431000},
		{"id":"a2","uid":"uid-json-2","email":"a2@x","access_token":"at","refresh_token":"rt","expires_at":0}
	]`
	body := `{"name":"accounts.json","data":"` + base64.StdEncoding.EncodeToString([]byte(accounts)) + `"}`
	code, resp := postImportJSON(t, pn, body)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, resp)
	}
	for _, want := range []string{`"imported":2`, `"skipped":0`, `"ok":true`} {
		if !strings.Contains(resp, want) {
			t.Fatalf("resp missing %s: %s", want, resp)
		}
	}
	for _, uid := range []string{"uid-json-1", "uid-json-2"} {
		if _, err := os.Stat(filepath.Join(authDir, "workbuddy-"+uid+".json")); err != nil {
			t.Fatalf("auth file missing for %s: %v", uid, err)
		}
	}
}

func TestImportCockpitJSONBodyBadData(t *testing.T) {
	pn, _ := newImportTestPanel(t)
	if code, resp := postImportJSON(t, pn, `{"name":"x.json","data":"not-base64!!"}`); code != 400 {
		t.Fatalf("bad base64: status=%d body=%s", code, resp)
	}
	if code, resp := postImportJSON(t, pn, `{"name":"x.json"} `); code != 400 {
		t.Fatalf("empty data: status=%d body=%s", code, resp)
	}
}
