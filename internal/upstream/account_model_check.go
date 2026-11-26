package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// FetchAccountModelNames fetches the selected account's chat catalog without
// accepting another account's realm-wide cached catalog.
func (c *Client) FetchAccountModelNames(ctx context.Context, a *auth.Auth) ([]string, error) {
	var names []string
	if a.IsGlobal() {
		var infos []ModelInfo
		var err error
		names, infos, _, _, err = c.probeGlobalModels(ctx, a)
		if err != nil {
			return nil, err
		}
		c.storeModelRates(a.Realm(), infos)
	} else {
		infos, err := c.FetchModelsContext(ctx, a)
		if err != nil {
			return nil, err
		}
		for _, info := range infos {
			names = append(names, info.ID)
		}
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// CheckModelReply performs one short chat and checks both HTTP and stream-level
// rejection. Diagnostic logs omit raw upstream bodies and transport details.
func (c *Client) CheckModelReply(ctx context.Context, a *auth.Auth, model string) (map[string]any, int, []byte, error) {
	body, _ := json.Marshal(map[string]any{
		"model": model, "stream": true, "max_tokens": 16,
		"messages": []map[string]string{{"role": "user", "content": "Reply with OK."}},
	})
	rc, status, raw, err := c.chatStreamContext(ctx, a, body, "", ChatMeta{}, false)
	if err != nil || rc == nil {
		return nil, status, raw, err
	}
	defer rc.Close()
	reader := &io.LimitedReader{R: rc, N: (1 << 20) + 1}
	resp, err := aggregate(reader, true)
	if reader.N == 0 && err == nil {
		err = errors.New("oversized model response")
	}
	return resp, status, nil, err
}

// Catalog diagnostics need status and kind; the raw payload may contain
// account details and is unnecessary in errors surfaced by the panel.
func modelCatalogError(status int, raw []byte) *Error {
	return &Error{Kind: Classify(status, string(raw)), Status: status, Msg: fmt.Sprintf("model catalog status %d", status)}
}

func catalogBusinessError(code int, raw []byte) *Error {
	status := http.StatusBadRequest
	if code >= 400 && code < 600 {
		status = code
	}
	return &Error{Kind: Classify(status, string(raw)), Status: status, Msg: fmt.Sprintf("model catalog code=%d", code)}
}

func checkEnvelopeError(obj map[string]any, raw string) *Error {
	code := obj["code"]
	if obj["error"] == nil && (code == nil || code == float64(0) || code == "0") {
		return nil
	}
	status := http.StatusBadRequest
	if inner, ok := obj["error"].(map[string]any); ok {
		for _, key := range []string{"status", "status_code", "code"} {
			if v, ok := inner[key].(float64); ok && v >= 400 && v < 600 {
				status = int(v)
				break
			}
		}
	}
	kind := Classify(status, raw)
	if kind == ErrClient {
		if frameKind := FrameKind(raw); frameKind != ErrNone {
			kind = frameKind
		}
	}
	return &Error{Kind: kind, Status: status, Msg: raw}
}
