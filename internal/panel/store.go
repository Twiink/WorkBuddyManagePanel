// store.go 「数据检索」页接口：SQLite 中的请求记录与运行日志的检索、导出、清空。
package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logstore"
)

func (p *Panel) storeStats(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LogStore == nil {
		writeJSON(w, http.StatusOK, logstore.Stats{})
		return
	}
	writeJSON(w, http.StatusOK, p.cfg.LogStore.Stats(r.Context()))
}

func atoiDefault(v string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		return n
	}
	return def
}

func requestQueryOf(r *http.Request) logstore.RequestQuery {
	q := r.URL.Query()
	return logstore.RequestQuery{
		Q:       q.Get("q"),
		Outcome: q.Get("outcome"),
		From:    parseTimeParam(q.Get("from")),
		To:      parseTimeParam(q.Get("to")),
		Limit:   atoiDefault(q.Get("limit"), 100),
		Offset:  atoiDefault(q.Get("offset"), 0),
	}
}

func logQueryOf(r *http.Request) logstore.LogQuery {
	q := r.URL.Query()
	return logstore.LogQuery{
		Q:      q.Get("q"),
		Ch:     q.Get("ch"),
		Level:  q.Get("level"),
		From:   parseTimeParam(q.Get("from")),
		To:     parseTimeParam(q.Get("to")),
		Limit:  atoiDefault(q.Get("limit"), 100),
		Offset: atoiDefault(q.Get("offset"), 0),
	}
}

func storeErr(w http.ResponseWriter, err error) {
	if errors.Is(err, logstore.ErrDisabled) {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeErr(w, http.StatusInternalServerError, err.Error())
}

// storeRequests 检索请求记录：q（空格分词 AND）/ outcome / from / to / limit / offset。
func (p *Panel) storeRequests(w http.ResponseWriter, r *http.Request) {
	rq := requestQueryOf(r)
	rows, total, err := p.cfg.LogStore.Requests(r.Context(), rq)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows, "total": total})
}

// storeLogs 检索运行日志：q / ch / level / from / to / limit / offset。
func (p *Panel) storeLogs(w http.ResponseWriter, r *http.Request) {
	rows, total, err := p.cfg.LogStore.Logs(r.Context(), logQueryOf(r))
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows, "total": total})
}

// storeExport 按当前检索条件导出：kind=requests|logs，format=csv|jsonl。
// 面板经 fetch 取回 blob 再触发下载（API Key 走 Authorization 头，不能用裸链接）。
func (p *Panel) storeExport(w http.ResponseWriter, r *http.Request) {
	if !p.cfg.LogStore.Enabled() {
		storeErr(w, logstore.ErrDisabled)
		return
	}
	kind := r.URL.Query().Get("kind")
	format := r.URL.Query().Get("format")
	if format != "jsonl" {
		format = "csv"
	}
	if kind != "requests" && kind != "logs" {
		writeErr(w, http.StatusBadRequest, "kind 只能是 requests 或 logs")
		return
	}
	ext, ctype := ".csv", "text/csv; charset=utf-8"
	if format == "jsonl" {
		ext, ctype = ".jsonl", "application/x-ndjson; charset=utf-8"
	}
	name := fmt.Sprintf("wb-%s-%s%s", kind, time.Now().Format("20060102-150405"), ext)
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	var n int
	var err error
	if kind == "requests" {
		rq := requestQueryOf(r)
		n, err = p.cfg.LogStore.ExportRequests(r.Context(), w, rq, format)
	} else {
		n, err = p.cfg.LogStore.ExportLogs(r.Context(), w, logQueryOf(r), format)
	}
	// 已开始写响应体，出错只能记日志（客户端拿到的是截断文件）。
	if err != nil {
		log.Printf("panel: 导出 %s 中断（已写 %d 条）: %v", kind, n, err)
	}
}

// storeClear 清空 SQLite 中的记录：body {"kind":"requests"|"logs"|"all"}。
func (p *Panel) storeClear(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体应为 {\"kind\": ...}")
		return
	}
	if err := p.cfg.LogStore.Clear(r.Context(), body.Kind); err != nil {
		if strings.HasPrefix(err.Error(), "未知") {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		storeErr(w, err)
		return
	}
	log.Printf("panel: 已清空 SQLite 存储（%s）", body.Kind)
	writeJSON(w, http.StatusOK, p.cfg.LogStore.Stats(r.Context()))
}
