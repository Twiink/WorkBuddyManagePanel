// bare_model_names_test.go 裸名模式（models.bare_names / models.preferred_realm）：
// /v1/models 两域合并输出裸名；裸名请求按目录选域，重名模型按首选域、选不到号回退。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// seedCNCatalog 预置 CN 模型目录缓存（不联网），测试结束清空。
func seedCNCatalog(t *testing.T, ids ...string) {
	t.Helper()
	infos := make([]upstream.ModelInfo, 0, len(ids))
	for _, id := range ids {
		infos = append(infos, upstream.ModelInfo{ID: id, Description: "desc-" + id})
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	})
}

func liveBare(preferred string) *livecfg.Holder {
	return livecfg.New(livecfg.Snapshot{BareModelNames: true, PreferredRealm: preferred})
}

func TestMergeBareModels(t *testing.T) {
	in := []map[string]any{
		{"id": "cn:deepseek-v4.1-flash", "description": "cn desc", "credits": "x0.1"},
		{"id": "cn:glm-5.2"},
		{"id": "global:gpt-5.4", "description": "gpt"},
		{"id": "global:deepseek-v4.1-flash", "description": "gl desc", "credits": "x0.5"},
	}
	byID := func(out []map[string]any) map[string]map[string]any {
		m := map[string]map[string]any{}
		for _, e := range out {
			m[e["id"].(string)] = e
		}
		return m
	}

	cn := byID(mergeBareModels(in, "cn"))
	if len(cn) != 3 {
		t.Fatalf("want 3 merged ids, got %v", cn)
	}
	ds := cn["deepseek-v4.1-flash"]
	if ds["credits"] != "x0.1" || ds["description"] != "[CN+Global] cn desc" {
		t.Errorf("cn-preferred dup entry = %v, want cn metadata tagged [CN+Global]", ds)
	}
	if r := ds["realms"].([]string); len(r) != 2 || r[0] != "cn" {
		t.Errorf("realms=%v want [cn global]", r)
	}
	if cn["gpt-5.4"]["description"] != "[Global] gpt" || cn["glm-5.2"]["description"] != "[CN]" {
		t.Errorf("single-realm tags wrong: %v / %v", cn["gpt-5.4"], cn["glm-5.2"])
	}

	gl := byID(mergeBareModels(in, "global"))
	ds = gl["deepseek-v4.1-flash"]
	if ds["credits"] != "x0.5" || ds["description"] != "[Global+CN] gl desc" {
		t.Errorf("global-preferred dup entry = %v, want global metadata tagged [Global+CN]", ds)
	}
	if in[0]["id"] != "cn:deepseek-v4.1-flash" {
		t.Error("mergeBareModels must not mutate input entries")
	}
}

func TestMergeBareModelsCaseInsensitiveIDs(t *testing.T) {
	in := []map[string]any{
		{"id": "cn:glm-5.3", "description": "cn desc"},
		{"id": "global:GLM-5.3", "description": "global desc"},
		{"id": "cn:GLM-5.3", "description": "cn duplicate"},
	}
	cases := []struct {
		preferred string
		id        string
		desc      string
		realms    []string
	}{
		{"cn", "glm-5.3", "[CN+Global] cn desc", []string{"cn", "global"}},
		{"global", "GLM-5.3", "[Global+CN] global desc", []string{"global", "cn"}},
	}
	for _, tc := range cases {
		out := mergeBareModels(in, tc.preferred)
		if len(out) != 1 {
			t.Fatalf("preferred=%s: want one case-insensitive model, got %v", tc.preferred, out)
		}
		if got := out[0]["id"]; got != tc.id {
			t.Errorf("preferred=%s: id=%v want %q", tc.preferred, got, tc.id)
		}
		if got := out[0]["description"]; got != tc.desc {
			t.Errorf("preferred=%s: description=%v want %q", tc.preferred, got, tc.desc)
		}
		realms, _ := out[0]["realms"].([]string)
		if len(realms) != len(tc.realms) {
			t.Fatalf("preferred=%s: realms=%v want %v", tc.preferred, realms, tc.realms)
		}
		for i := range tc.realms {
			if realms[i] != tc.realms[i] {
				t.Errorf("preferred=%s: realms=%v want %v", tc.preferred, realms, tc.realms)
				break
			}
		}
	}
}

func TestModelsBareNamesListing(t *testing.T) {
	seedCNCatalog(t, "glm-5.2", "deepseek-v4.1-flash")
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, `{"data":[]}`, false })
	p := newRealmPool(t)

	get := func(h *Handler) []string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
		var resp struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v body=%s", err, rec.Body)
		}
		ids := make([]string, 0, len(resp.Data))
		for _, d := range resp.Data {
			ids = append(ids, d.ID)
		}
		return ids
	}

	if ids := get(NewHandler(Config{Pool: p, Upstream: up})); !contains(ids, "cn:glm-5.2") {
		t.Errorf("default listing must keep prefixes: %v", ids)
	}
	ids := get(NewHandler(Config{Pool: p, Upstream: up, Live: liveBare("cn")}))
	if !contains(ids, "glm-5.2") || !contains(ids, "deepseek-v4.1-flash") {
		t.Errorf("bare listing missing bare ids: %v", ids)
	}
	for _, id := range ids {
		if strings.HasPrefix(id, "cn:") || strings.HasPrefix(id, "global:") {
			t.Errorf("bare listing leaked prefixed id %q", id)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// 目录未知（global 未探测）的重名候选：首选 global → 先打 global 号。
func TestBareModelPreferredGlobal(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: newRealmPool(t), Upstream: up, Live: liveBare("global")})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-gl"] != 1 || calls["Bearer at-cn"] != 0 {
		t.Errorf("calls=%v want global 1, cn 0 (preferred_realm=global)", calls)
	}
}

// 默认首选 cn；cn 整域不可用时裸名模式自动回退 global（无需 cross_domain_fallback）。
func TestBareModelPreferredCNFallsBack(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 200, sseOK, true
	})
	p := newRealmPool(t)
	p.Cooldown("cn1", pool.CoolSoft, time.Hour, "test")
	h := NewHandler(Config{Pool: p, Upstream: up, Live: liveBare("")})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (bare mode should fall back to global)", rec.Code, rec.Body)
	}
	if calls["Bearer at-gl"] != 1 {
		t.Errorf("calls=%v want global 1", calls)
	}
}

// 模型只在 CN 目录（global 目录未知时不能断定）：首选 global 也照常先试 global、
// 但 cn 目录已知且含、global 隐藏 → 直达 cn。
func TestBareModelSingleRealmRoutesDirect(t *testing.T) {
	seedCNCatalog(t, "glm-5.2")
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 200, sseOK, true
	})
	live := livecfg.New(livecfg.Snapshot{
		BareModelNames: true, PreferredRealm: "global",
		HiddenModels: livecfg.NormalizeHiddenModels([]string{"global:glm-5.2"}),
	})
	h := NewHandler(Config{Pool: newRealmPool(t), Upstream: up, Live: live})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-cn"] != 1 || calls["Bearer at-gl"] != 0 {
		t.Errorf("calls=%v want cn 1, global 0 (only cn serves it)", calls)
	}
}

// 显式前缀在裸名模式下仍固定该域。
func TestBareModeExplicitPrefixPinned(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: newRealmPool(t), Upstream: up, Live: liveBare("global")})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK || calls["Bearer at-cn"] != 1 || calls["Bearer at-gl"] != 0 {
		t.Fatalf("code=%d calls=%v want cn only", rec.Code, calls)
	}
}
