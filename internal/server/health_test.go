package server

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/health"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestApplyErrorPolicyRecordsHealth 每次处置都进健康台账，含不罚账号的类型
// （内容拦截等）——台账回答「错在哪」，与是否冷却无关。
func TestApplyErrorPolicyRecordsHealth(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	tr := health.New("")
	h := NewHandler(Config{Pool: p, Health: tr})

	h.applyErrorPolicy("u1", upstream.ErrSoftRate, "rate", "m", &upstream.Error{Kind: upstream.ErrSoftRate, Status: 429})
	h.applyErrorPolicy("u1", upstream.ErrContentBlocked, "blocked", "m", nil)

	v := tr.Snapshot([]string{"u1"}).Accounts[0]
	if v.Err24h != 2 {
		t.Fatalf("err_24h=%d want 2", v.Err24h)
	}
	if v.LastErr == nil || v.LastErr.Kind != "content_blocked" {
		t.Fatalf("last_err=%+v want content_blocked", v.LastErr)
	}
	kinds := map[string]int64{}
	for _, k := range v.Kinds24h {
		kinds[k.Kind] = k.Count
	}
	if kinds["soft_rate"] != 1 || kinds["content_blocked"] != 1 {
		t.Fatalf("kinds=%v", kinds)
	}
}

// TestApplyErrorPolicyNilHealth 未注入台账时照常处置（旁路可选）。
func TestApplyErrorPolicyNilHealth(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p})
	h.applyErrorPolicy("u1", upstream.ErrServer, "", "", nil)
}
