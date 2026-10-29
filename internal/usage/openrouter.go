package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// OpenRouterURL OpenRouter 模型目录（公开接口，无需密钥）。
const OpenRouterURL = "https://openrouter.ai/api/v1/models"

// orHTTP 拉取用的 HTTP 客户端（与上游聊天链路完全独立）。
var orHTTP = &http.Client{Timeout: 45 * time.Second}

// FetchOpenRouter 拉取 OpenRouter 模型目录，返回 id → pricing（字符串，每 token 美元）。
// ctx 取消（进程退出）时立即中止，不让关停等完一次 45 秒的网络请求。
func FetchOpenRouter(ctx context.Context) (map[string]map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, OpenRouterURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "workbuddy-manage-panel-pricing/1.0")
	resp, err := orHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter: HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID      string         `json:"id"`
			Pricing map[string]any `json:"pricing"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("openrouter: 解析失败: %w", err)
	}
	out := make(map[string]map[string]string, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID == "" {
			continue
		}
		p := map[string]string{}
		for k, v := range m.Pricing {
			if s, ok := v.(string); ok {
				p[k] = s
			}
		}
		out[m.ID] = p
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("openrouter: 目录为空")
	}
	return out, nil
}

// builtinMapping 名字与 OpenRouter 写法不同、自动匹配找不到的模型（与
// workbuddy2api-hub 的 OVERRIDES 同口径）。能按名字匹配的模型不要写进来。
var builtinMapping = map[string]string{
	"hy4-preview-f":        "tencent/hy4-preview",
	"hy4-preview-dev":      "tencent/hy4-preview",
	"hy4-preview-x":        "tencent/hy4-preview",
	"hy3-x":                "tencent/hy3",
	"kimi-k3-1":            "moonshotai/kimi-k3",
	"kimi-k2.7":            "moonshotai/kimi-k2.7-code",
	"glm-5.0-turbo":        "z-ai/glm-5-turbo",
	"deepseek-v3-1":        "deepseek/deepseek-chat-v3.1",
	"deepseek-v3-1-volc":   "deepseek/deepseek-chat-v3.1",
	"deepseek-v3-2-volc":   "deepseek/deepseek-v3.2",
	"deepseek-v3-0324":     "deepseek/deepseek-chat-v3-0324",
	"hunyuan-2.0-instruct": "tencent/hunyuan-a13b-instruct",
	"hunyuan-chat":         "tencent/hunyuan-a13b-instruct",
}

// variantSuffixes 渠道/节点后缀：剥掉后按基名重新匹配（仍要求唯一命中）。
// -f / -dev / -x 不在此列：它们在上游是不同档位，单价不同，继承会把价压低。
var variantSuffixes = []string{"-lkeap", "-taiji", "-volc", "-sg"}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]`)

func normalizeName(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return nonAlnum.ReplaceAllString(strings.ToLower(s), "")
}

// perMillion 每 token 美元（字符串）→ 每百万 token 美元；缺失/非法返回 ok=false。
func perMillion(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return math.Round(v*1e6*1e6) / 1e6, true // 保留 6 位小数，消掉浮点尾差
}

// BuildPriceTable 把 OpenRouter 目录匹配到本地模型名，返回价格表与未匹配清单。
// 解析顺序：手填映射 → 内置映射 → 归一化名字唯一命中 → 剥渠道后缀后重走前三步。
// 宁可漏也不错：名字匹配到多个候选时不定价。
func BuildPriceTable(or map[string]map[string]string, ids []string, mapping map[string]string) (priceTable, []string) {
	byNorm := map[string][]string{}
	for id := range or {
		if strings.Contains(id, ":") {
			continue // :free / :batch 等变体不参与名字匹配
		}
		n := normalizeName(id)
		byNorm[n] = append(byNorm[n], id)
	}
	once := func(id string) (string, string) {
		if ref := mapping[id]; ref != "" {
			if _, ok := or[ref]; ok {
				return ref, "override"
			}
		}
		if ref := builtinMapping[id]; ref != "" {
			if _, ok := or[ref]; ok {
				return ref, "override"
			}
		}
		if c := byNorm[normalizeName(id)]; len(c) == 1 {
			return c[0], "exact"
		}
		return "", ""
	}
	t := priceTable{
		Source:      "OpenRouter 模型级公布价（动态刷新）",
		GeneratedAt: time.Now().Format("2006-01-02T15:04:05"),
		UsdCny:      defaultUsdCny,
		Unit:        1_000_000,
		Models:      map[string]modelPrice{},
	}
	var unmatched []string
	for _, id := range ids {
		ref, via := once(id)
		if ref == "" {
			for _, suf := range variantSuffixes {
				if base := strings.TrimSuffix(id, suf); base != id && base != "" {
					if ref, _ = once(base); ref != "" {
						via = "variant"
						break
					}
				}
			}
		}
		if ref == "" {
			unmatched = append(unmatched, id)
			continue
		}
		p := or[ref]
		in, okIn := perMillion(p["prompt"])
		out, okOut := perMillion(p["completion"])
		if !okIn || !okOut {
			unmatched = append(unmatched, id)
			continue
		}
		mp := modelPrice{Input: in, Output: out, ORID: ref, Via: via}
		if hit, ok := perMillion(p["input_cache_read"]); ok {
			mp.CacheHit = &hit
		}
		t.Models[id] = mp
	}
	return t, unmatched
}
