package panel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// SetModelChecker wires the gateway's single-account checker at startup,
// before accepting requests on either the desktop or HTTP listener.
func (p *Panel) SetModelChecker(check func(context.Context, *auth.Auth, string) server.ModelCheckResult) {
	p.checkModel = check
}

func checkUnavailable(st pool.Status) string {
	switch {
	case st.RiskHold:
		return "此账号处于风控隔离中"
	case st.Disabled:
		return "此账号已禁用"
	case st.Cooling || time.Now().Before(st.DegradeUntil):
		return "此账号正在冷却，请稍后再检测"
	default:
		return ""
	}
}

func (p *Panel) accountModelCheck(w http.ResponseWriter, r *http.Request) {
	if p.checkModel == nil || p.cfg.Pool == nil || p.cfg.Upstream == nil {
		writeErr(w, http.StatusServiceUnavailable, "账号检测服务未就绪")
		return
	}
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	st, found := p.cfg.Pool.Status(uid)
	if !found || a == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	if reason := checkUnavailable(st); reason != "" {
		writeErr(w, http.StatusConflict, reason)
		return
	}
	if auth.ResolveRealm(a.RealmStored(), a.DomainValue()) == "global" && (!auth.GlobalEnabled() || !p.cfg.Upstream.GlobalEnabled) {
		writeErr(w, http.StatusConflict, "国际站路由已关闭")
		return
	}
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "该账号已有检测或任务正在执行")
		return
	}
	defer p.unlockAccount(uid)

	ctx := r.Context()
	catalogCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	names, err := p.cfg.Upstream.FetchAccountModelNames(catalogCtx, a)
	cancel()
	if err != nil || len(names) == 0 {
		message := "未能读取该账号的模型目录"
		var ue *upstream.Error
		if errors.As(err, &ue) {
			switch {
			case ue.Status == http.StatusUnauthorized || ue.Kind == upstream.ErrSessionDead:
				message = "上游拒绝该账号的登录凭证，无法读取模型目录"
			case ue.Kind == upstream.ErrAccountFault:
				message = "上游限制了此账号，无法读取模型目录"
			case ue.Status == http.StatusForbidden:
				message = "上游拒绝此账号访问模型目录"
			case ue.Kind == upstream.ErrSoftRate:
				message = "上游已限流，无法读取模型目录"
			}
		}
		if errors.Is(err, context.DeadlineExceeded) {
			message = "读取账号模型目录超时"
		}
		writeErr(w, http.StatusBadGateway, message)
		return
	}

	models := make([]string, len(names))
	for i, model := range names {
		models[i] = a.Realm() + ":" + model
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	enc := json.NewEncoder(w)
	rc := http.NewResponseController(w)
	send := func(event any) bool {
		if enc.Encode(event) != nil {
			return false
		}
		err := rc.Flush()
		// Some desktop response writers buffer the completed response.
		return err == nil || errors.Is(err, http.ErrNotSupported)
	}
	if !send(map[string]any{"type": "start", "models": models}) {
		return
	}
	counts := map[string]int{}
	stop := ""
	for i, name := range names {
		if ctx.Err() != nil {
			return
		}
		if stop == "" {
			if st, ok := p.cfg.Pool.Status(uid); ok {
				stop = checkUnavailable(st)
			} else {
				stop = "账号已移除"
			}
		}
		result := server.ModelCheckResult{Status: "skipped", Message: stop}
		if stop == "" {
			result = p.checkModel(ctx, a, name)
			if result.Stop {
				stop = result.Message
			}
		}
		result.Model = models[i]
		counts[result.Status]++
		if !send(struct {
			Type string `json:"type"`
			server.ModelCheckResult
		}{"result", result}) {
			return
		}
	}
	done := map[string]any{"type": "done", "passed": counts["passed"], "failed": counts["failed"], "skipped": counts["skipped"]}
	if stop != "" {
		done["stop_reason"] = stop
	}
	send(done)
}
