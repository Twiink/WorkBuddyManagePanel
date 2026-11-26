package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func newModelCheckPanel(t *testing.T, handler http.HandlerFunc) (*Panel, *pool.Pool) {
	t.Helper()
	fake := httptest.NewServer(handler)
	t.Cleanup(fake.Close)
	up := &upstream.Client{HTTP: fake.Client(), ChatBaseCN: fake.URL, ChatBaseGlobal: fake.URL, GlobalEnabled: true}
	accounts := pool.New("")
	accounts.Add(&auth.Auth{UID: "chosen", AccessToken: "sample-one"})
	accounts.Add(&auth.Auth{UID: "other", AccessToken: "sample-two"})
	p := New(Config{Pool: accounts, Upstream: up, APIKey: "fixture"})
	gateway := server.NewHandler(server.Config{Pool: accounts, Upstream: up, RiskProbeEnabled: true, RiskQuarantine: time.Hour})
	p.SetModelChecker(gateway.CheckAccountModel)
	return p, accounts
}

func postModelCheck(ctx context.Context, p *Panel) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, "POST", "/panel/api/accounts/chosen/model-check", nil)
	req.Header.Set("Authorization", "Bearer fixture")
	p.ServeHTTP(rec, req)
	return rec
}

func checkCatalog(w http.ResponseWriter) {
	io.WriteString(w, `{"code":0,"data":{"models":[{"id":"d","maxOutputTokens":4096},{"id":"b","maxOutputTokens":4096},{"id":"c","maxOutputTokens":4096},{"id":"a","maxOutputTokens":4096}],"agents":[{"name":"cli","models":["a","b","c","d"]}]}}`)
}

func TestAccountModelCheckSelectedAccountAndRiskStop(t *testing.T) {
	var calls []string
	var mu sync.Mutex
	p, accounts := newModelCheckPanel(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sample-one" {
			t.Error("requested another account")
			http.Error(w, "denied", 401)
			return
		}
		if r.Method == "GET" {
			checkCatalog(w)
			return
		}
		var body struct {
			Model     string
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.MaxTokens != 16 {
			t.Error("unexpected check request")
		}
		mu.Lock()
		calls = append(calls, body.Model)
		mu.Unlock()
		switch body.Model {
		case "a":
			w.WriteHeader(400)
			io.WriteString(w, `{"code":11158,"msg":"not allowed for user type"}`)
		case "b":
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}],\"usage\":{\"total_tokens\":8}}\n\ndata: [DONE]\n\n")
		case "c":
			io.WriteString(w, "data: {\"error\":{\"code\":11140,\"message\":\"request illegal\",\"detail\":\"private fixture text\"}}\n\ndata: [DONE]\n\n")
		default:
			t.Error("continued after account-wide restriction")
		}
	})
	rec := postModelCheck(t.Context(), p)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "private fixture text") {
		t.Fatal("unexpected diagnostic response", rec.Code)
	}
	dec := json.NewDecoder(rec.Body)
	var start struct {
		Type   string
		Models []string
	}
	if dec.Decode(&start) != nil || start.Type != "start" || !slices.Equal(start.Models, []string{"cn:a", "cn:b", "cn:c", "cn:d"}) {
		t.Fatal(start)
	}
	for i, status := range []string{"failed", "passed", "failed", "skipped"} {
		var event struct{ Type, Model, Status, Message string }
		if dec.Decode(&event) != nil || event.Type != "result" || event.Model != start.Models[i] || event.Status != status {
			t.Fatalf("unexpected event %+v", event)
		}
	}
	var done struct {
		Type                    string
		StopReason              string `json:"stop_reason"`
		Passed, Failed, Skipped int
	}
	if dec.Decode(&done) != nil || done.Type != "done" || done.Passed != 1 || done.Failed != 2 || done.Skipped != 1 || !strings.Contains(done.StopReason, "11140") {
		t.Fatal(done)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(calls, []string{"a", "b", "c"}) {
		t.Fatal(calls)
	}
	st, _ := accounts.Status("chosen")
	if !st.RiskHold || st.InFlight != 0 || st.SuccessCount != 1 {
		t.Fatal("missing quarantine or released in-flight slot")
	}
	if postModelCheck(t.Context(), p).Code != 409 {
		t.Fatal("quarantined account started another check")
	}
}

func TestAccountModelCheckCancellationAndConcurrency(t *testing.T) {
	first := make(chan struct{})
	var calls atomic.Int32
	p, accounts := newModelCheckPanel(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			checkCatalog(w)
			return
		}
		io.Copy(io.Discard, r.Body)
		if calls.Add(1) != 1 {
			t.Error("issued another model request")
			return
		}
		close(first)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	end := make(chan struct{})
	go func() { defer close(end); postModelCheck(ctx, p) }()
	select {
	case <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("no model request")
	}
	if rec := postModelCheck(t.Context(), p); rec.Code != 409 {
		t.Fatal(rec.Code)
	}
	cancel()
	select {
	case <-end:
	case <-time.After(3 * time.Second):
		t.Fatal("did not cancel")
	}
	st, _ := accounts.Status("chosen")
	if st.InFlight != 0 || !p.tryLockAccount("chosen") {
		t.Fatal("check still holds account")
	}
	p.unlockAccount("chosen")
}

func TestAccountModelCheckCatalogRejection(t *testing.T) {
	p, _ := newModelCheckPanel(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("model invoked after catalog rejection")
		}
		w.WriteHeader(401)
		fmt.Fprint(w, "fixture private response")
	})
	rec := postModelCheck(t.Context(), p)
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "登录凭证") || strings.Contains(rec.Body.String(), "fixture private response") {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

func TestAccountModelCheckGlobalUsesOwnCatalog(t *testing.T) {
	var chatCalls atomic.Int32
	p, accounts := newModelCheckPanel(t, func(w http.ResponseWriter, r *http.Request) {
		id := "other-model"
		if r.Header.Get("Authorization") == "Bearer sample-one" {
			id = "chosen-model"
		}
		if r.Method == http.MethodGet {
			fmt.Fprintf(w, `{"code":0,"data":{"models":[{"id":%q,"maxOutputTokens":4096}]}}`, id)
			return
		}
		var body struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "chosen-model" || id != "chosen-model" {
			t.Error("wrong model or account")
		}
		chatCalls.Add(1)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n")
	})
	for _, uid := range []string{"chosen", "other"} {
		_, err := auth.BackfillRealmFor(accounts.AuthByUID(uid), "global")
		if err != nil {
			t.Fatal(err)
		}
	}
	accounts.Pause("chosen")
	old := p.cfg.Upstream.FetchGlobalModels(accounts.AuthByUID("other"))
	if !slices.Equal(old, []string{"other-model"}) {
		t.Fatal(old)
	}
	rec := postModelCheck(t.Context(), p)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "global:chosen-model") || strings.Contains(rec.Body.String(), "other-model") || chatCalls.Load() != 1 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	st, _ := accounts.Status("chosen")
	if !st.Paused {
		t.Fatal("diagnostics resumed traffic routing")
	}
}
