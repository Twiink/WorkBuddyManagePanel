package pool

import (
	"fmt"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 份额上限：单号吃满 2× 公平份额后，新会话必须落到其他号。
func TestShareCapSpreadsLoad(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	for _, uid := range []string{"a", "b", "c", "d", "e", "f"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.SetAccountShareCap(2.0)
	p.SetRandomSource(func(n int64) int64 { return 0 }) // 恒选权重最高/字典序最前

	counts := map[string]int{}
	for i := 0; i < 120; i++ {
		a := p.Pick()
		if a == nil {
			t.Fatalf("iter %d: pick nil（份额上限不应把整池拦空）", i)
		}
		counts[a.UID]++
	}
	if len(counts) != 6 {
		t.Fatalf("counts=%v want all 6 accounts used（份额上限应把流量摊开）", counts)
	}
	for uid, c := range counts {
		if c > 60 {
			t.Fatalf("acct=%s count=%d 超过 2× 公平份额（120/6*2=40 附近）", uid, c)
		}
	}
}

// 28 号池 1000 次选号：所有账号都应参与，单号不超过 2× 公平份额太多。
func TestShareCapSpreads1000Across28(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	defer p.Close()
	for i := 0; i < 28; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("u%02d", i)})
	}
	p.SetAccountShareCap(2.0)
	p.SetRandomSource(func(int64) int64 { return 0 })

	counts := map[string]int{}
	for i := 0; i < 1000; i++ {
		a := p.Pick()
		if a == nil {
			t.Fatalf("iter %d: pick nil", i)
		}
		counts[a.UID]++
	}
	if len(counts) != 28 {
		t.Fatalf("used %d/28 accounts: %v", len(counts), counts)
	}
	fair := float64(1000) / 28
	maxAllowed := int(fair*2) + 1 // 冷启动门槛附近允许一个边界请求
	for uid, n := range counts {
		if n > maxAllowed {
			t.Fatalf("acct=%s requests=%d > %d (2x fair share %.1f): %v", uid, n, maxAllowed, fair, counts)
		}
	}
}

// 池子小于 4 个号时份额限制不生效（2 个号各 50% 就是公平份额）。
func TestShareCapDisabledForTinyPool(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetAccountShareCap(2.0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	for i := 0; i < 20; i++ {
		if p.Pick() == nil {
			t.Fatalf("iter %d: tiny pool must never be blocked by share cap", i)
		}
	}
}

// 粘性命中不做份额拦截（把已绑定会话换号会造成会话跨号）。
func TestShareCapDoesNotBlockSticky(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	for _, uid := range []string{"a", "b", "c", "d", "e"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.SetAccountShareCap(1.0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	for i := 0; i < 200; i++ {
		if a := p.PickByUIDForModel("a", ""); a == nil {
			t.Fatalf("iter %d: sticky account must stay selectable regardless of share", i)
		}
	}
}
