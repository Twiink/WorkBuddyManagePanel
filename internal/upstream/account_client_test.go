package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestAccountProxyRoutesRequest(t *testing.T) {
	var hits atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if got := r.Header.Get("X-Test-Proxy"); got != "yes" {
			t.Errorf("header X-Test-Proxy=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer proxy.Close()

	c := New()
	tr := &http.Transport{}
	c.HTTP.Transport = tr
	c.ChatHTTP.Transport = tr
	a := &auth.Auth{UID: "u1", Proxy: proxy.URL}

	req, err := http.NewRequest(http.MethodGet, "http://upstream.invalid/v2/report", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-Proxy", "yes")
	resp, err := c.doAccountRequest(a, req, false)
	if err != nil {
		t.Fatalf("doAccountRequest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if hits.Load() != 1 {
		t.Fatalf("proxy hits=%d want 1", hits.Load())
	}
}

func TestAccountProxyIsCachedAndSeparated(t *testing.T) {
	c := New()
	tr := &http.Transport{}
	c.HTTP.Transport = tr
	c.ChatHTTP.Transport = tr

	a := &auth.Auth{UID: "u1", Proxy: "http://127.0.0.1:18081"}
	b := &auth.Auth{UID: "u2", Proxy: "http://127.0.0.1:18082"}
	a1, err := c.accountHTTPClient(a, false)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := c.accountHTTPClient(a, false)
	if err != nil {
		t.Fatal(err)
	}
	b1, err := c.accountHTTPClient(b, false)
	if err != nil {
		t.Fatal(err)
	}
	if a1 != a2 {
		t.Fatal("same proxy should reuse the cached client")
	}
	if a1 == b1 {
		t.Fatal("different proxy URLs must not share a client")
	}
	if a1.Transport == b1.Transport {
		t.Fatal("different proxy URLs must not share a transport")
	}
}

func TestAccountProxyFailsClosed(t *testing.T) {
	c := New()
	tr := &http.Transport{}
	c.HTTP.Transport = tr
	c.ChatHTTP.Transport = tr
	for _, proxy := range []string{
		"not a url",
		"file:///tmp/proxy.sock",
		"http://",
	} {
		a := &auth.Auth{UID: "u1", Proxy: proxy}
		if _, err := c.accountHTTPClient(a, false); err == nil {
			t.Errorf("proxy %q should be rejected", proxy)
		} else if strings.Contains(err.Error(), proxy) {
			t.Errorf("error must not echo proxy credentials/URL: %v", err)
		}
	}
}

func TestNoAccountProxyKeepsSharedClient(t *testing.T) {
	c := New()
	a := &auth.Auth{UID: "u1"}
	got, err := c.accountHTTPClient(a, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != c.HTTP {
		t.Fatal("empty proxy must reuse the default HTTP client")
	}
}
