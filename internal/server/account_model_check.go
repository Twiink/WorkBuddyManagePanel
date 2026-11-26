package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/health"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ModelCheckResult contains only display-safe diagnostics. Stop prevents later
// calls in the series after an account-wide rejection.
type ModelCheckResult struct {
	Model      string `json:"model"`
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
	ElapsedMS  int64  `json:"elapsed_ms"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Stop       bool   `json:"-"`
}

// CheckAccountModel sends one bounded request using exactly the selected account.
// There is no failover, retry, credential renewal or rate-limit fallback.
func (h *Handler) CheckAccountModel(ctx context.Context, a *auth.Auth, model string) (result ModelCheckResult) {
	result.Model = a.Realm() + ":" + model
	result.Status = "skipped"
	if _, hidden := h.loadLive().HiddenModels[result.Model]; hidden {
		result.Message = "已在网关中停用此模型"
		return
	}
	if reason := h.cfg.Pool.AcquireForModelCheck(a, model); reason != "" {
		result.Message = reason
		return
	}
	defer h.cfg.Pool.Release(a.UID)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	started := time.Now()
	result.Status = "failed"
	var delta pool.TokenUsageDelta
	var credit float64
	var hasCredit bool
	st := &chatStat{}
	defer func() {
		result.ElapsedMS = time.Since(started).Milliseconds()
		h.recordModelAttempt(st, a.UID, model, h.cfg.Upstream.ModelRate(a.Realm(), model), delta, credit, hasCredit, started, 0)
	}()
	resp, status, raw, err := h.cfg.Upstream.CheckModelReply(ctx, a, model)
	result.HTTPStatus = status
	if err != nil || resp == nil {
		var ue *upstream.Error
		if errors.As(err, &ue) {
			if len(raw) == 0 {
				raw = []byte(ue.Msg)
			}
			result.Message, result.Stop = checkFailure(ue.Kind, ue.Status, string(raw))
			h.applyErrorPolicyWithMessage(a.UID, ue.Kind, string(raw), model, ue, result.Message)
		} else if ctx.Err() != nil {
			result.Message = "请求已取消或超时"
		} else if status == 0 {
			result.Message = "无法连接上游"
			h.cfg.Health.Failure(a.UID, health.KindTransport, 0, model, result.Message)
		} else {
			result.Message = "上游未返回有效的模型答复"
		}
		return
	}
	delta = usageDeltaFromResponse(resp)
	if usage, ok := resp["usage"].(map[string]any); ok {
		credit, hasCredit = usage["credit"].(float64)
		st.cacheHit, st.cacheMiss, st.hasCache = cacheTokensFromUsage(usage)
	}
	if hasCredit && delta.TotalTokens > 0 {
		h.cfg.Pool.NoteModelCost(a.UID, model, credit, int(delta.TotalTokens))
	}
	h.cfg.Pool.NoteSuccess(a.UID)
	h.cfg.Health.Success(a.UID)
	h.cfg.Pool.BlockModelClear(a.UID, model)
	result.Status = "passed"
	return
}

func checkFailure(kind upstream.ErrKind, status int, raw string) (message string, stop bool) {
	switch kind {
	case upstream.ErrSessionDead:
		return "登录会话已失效，需要重新登录", true
	case upstream.ErrAccountFault:
		if upstream.IsAccountRestricted(raw) {
			return "上游已限制此账号（11140）", true
		}
		return "账号授权或配额状态异常", true
	case upstream.ErrHardCredit:
		return "账号余额不足", true
	case upstream.ErrSoftRate:
		if upstream.IsModelRateLimit(raw) {
			if _, ok := upstream.ParseRateReset(raw); ok {
				return "该模型已达到用量限制", false
			}
		}
		return "上游限流，已停止后续请求", true
	case upstream.ErrChannelRefused, upstream.ErrWafBlock:
		return "上游拒绝此请求通道", true
	case upstream.ErrModelBlocked:
		return "该账号无法使用此模型", false
	}
	switch status {
	case http.StatusUnauthorized:
		return "上游拒绝该账号的登录凭证", true
	case http.StatusForbidden:
		return "上游拒绝访问", true
	}
	if kind == upstream.ErrServer || kind == upstream.ErrUpstreamBusy {
		return "上游服务暂时不可用", false
	}
	return "上游拒绝本次模型请求", false
}
