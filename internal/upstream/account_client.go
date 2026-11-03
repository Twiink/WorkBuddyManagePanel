package upstream

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// accountProxyClients 一个代理 URL 对应的短 RPC / chat 客户端对。
// 两个客户端共享同一 Transport，保持连接池与超时语义一致。
type accountProxyClients struct {
	httpClient *http.Client
	chatClient *http.Client
	transport  *http.Transport
	err        error
}

// doAccountRequest 是所有账号级上游请求的唯一出口：
//  1. 先经过账号级 pacer；
//  2. 按 auth.proxy 选择该账号专用 Transport（空 = 进程默认）；
//  3. 传输失败时清掉对应 Transport 的空闲连接池。
//
// 统一出口避免某些低频接口绕过节流，也避免代理只接到 chat 而漏掉签到/余额等
// billing 请求，导致同一账号从两个出口 IP 出现。
func (c *Client) doAccountRequest(a *auth.Auth, req *http.Request, chat bool) (*http.Response, error) {
	if c == nil || req == nil {
		return nil, errors.New("nil upstream client or request")
	}
	if a != nil {
		if err := c.WaitTurn(req.Context(), a.UID); err != nil {
			return nil, err
		}
	}
	cl, err := c.accountHTTPClient(a, chat)
	if err != nil {
		return nil, err
	}
	resp, err := cl.Do(req)
	if err != nil {
		roundTripCloseIdle(cl.Transport)
		return nil, err
	}
	return resp, nil
}

// accountHTTPClient 返回该账号应使用的 HTTP 客户端。
// proxy 为空时完全复用既有 HTTP/ChatHTTP，保持测试注入和零配置行为不变。
func (c *Client) accountHTTPClient(a *auth.Auth, chat bool) (*http.Client, error) {
	if c == nil {
		return nil, errors.New("nil upstream client")
	}
	raw := ""
	if a != nil {
		raw = strings.TrimSpace(a.ProxyValue())
	}
	if raw == "" {
		if chat {
			return c.chatHTTP(), nil
		}
		if c.HTTP == nil {
			return nil, errors.New("upstream HTTP client is nil")
		}
		return c.HTTP, nil
	}

	key, proxyURL, err := normalizeAccountProxy(raw)
	if err != nil {
		return nil, err
	}
	c.accountProxyMu.Lock()
	if c.accountProxyClients == nil {
		c.accountProxyClients = map[string]*accountProxyClients{}
	}
	if cached := c.accountProxyClients[key]; cached != nil {
		c.accountProxyMu.Unlock()
		if chat {
			return cached.chatClient, cached.err
		}
		return cached.httpClient, cached.err
	}
	c.accountProxyMu.Unlock()

	created := c.newAccountProxyClients(proxyURL)

	c.accountProxyMu.Lock()
	if existing := c.accountProxyClients[key]; existing != nil {
		c.accountProxyMu.Unlock()
		if created.transport != nil {
			created.transport.CloseIdleConnections()
		}
		if chat {
			return existing.chatClient, existing.err
		}
		return existing.httpClient, existing.err
	}
	c.accountProxyClients[key] = created
	c.accountProxyMu.Unlock()

	if created.err != nil {
		return nil, created.err
	}
	if chat {
		return created.chatClient, nil
	}
	return created.httpClient, nil
}

// normalizeAccountProxy 校验并规范化账号代理 URL。
// 错误信息不回显原始 URL，避免把 user:password@host 写进日志或面板。
func normalizeAccountProxy(raw string) (string, *url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", nil, errors.New("account proxy: invalid URL")
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", nil, fmt.Errorf("account proxy: unsupported scheme %q", scheme)
	}
	u.Scheme = scheme
	return u.String(), u, nil
}

// newAccountProxyClients 克隆默认 Transport，并把 Proxy 固定为该账号代理。
// 克隆而不是修改共享 Transport：不同账号绝不能因并发请求互相覆盖出口设置。
func (c *Client) newAccountProxyClients(proxyURL *url.URL) *accountProxyClients {
	base := c.chatHTTP()
	var source http.RoundTripper
	if base != nil {
		source = base.Transport
	}
	if source == nil && c.HTTP != nil {
		source = c.HTTP.Transport
	}
	tr, ok := source.(*http.Transport)
	if !ok || tr == nil {
		return &accountProxyClients{err: errors.New("account proxy: default transport is not *http.Transport")}
	}
	proxyTr := tr.Clone()
	proxyTr.Proxy = http.ProxyURL(proxyURL)

	var httpTimeout time.Duration
	if c.HTTP != nil {
		httpTimeout = c.HTTP.Timeout
	}
	chatTimeout := httpTimeout
	if c.ChatHTTP != nil {
		chatTimeout = c.ChatHTTP.Timeout
	}
	return &accountProxyClients{
		httpClient: &http.Client{Timeout: httpTimeout, Transport: proxyTr},
		chatClient: &http.Client{Timeout: chatTimeout, Transport: proxyTr},
		transport:  proxyTr,
	}
}
