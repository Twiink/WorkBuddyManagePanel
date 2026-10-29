// Package logstore 把请求记录与运行日志持久化到 SQLite，供面板检索与导出。
//
// 写入路径：reqlog.Recorder 与面板日志环把每条记录交给 AppendRequest/AppendLog，
// 二者只做一次非阻塞的 channel 投递（队列满即丢弃并计数），真正的 INSERT 由唯一
// 的后台协程批量在一个事务里完成——慢盘或一次大导出都不会拖住模型请求。
//
// 存储内容与 JSONL 归档同口径：只有请求元数据（不含提示词、响应正文与凭证）和
// 面板「运行日志」里本来就能看到的日志行。
//
// 三个保留上限互相独立，0 = 该维度不限：
//   - RetentionDays 删除早于 N 天的记录
//   - MaxRows       每张表只保留最新 N 行
//   - MaxMB         文件超过 N MiB 时按比例删最旧的记录并 VACUUM 收缩
//
// 驱动是纯 Go 的 modernc.org/sqlite：发布构建一律 CGO_ENABLED=0。
package logstore

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

const (
	queueSize     = 4096
	batchMax      = 512
	trimInterval  = 10 * time.Second
	bytesInterval = time.Minute
	textMax       = 64 << 10 // 单行日志上限：防止误把整段响应打进日志把库撑爆
	// QueryMax 面板单页上限；ExportMax 单次导出上限。
	QueryMax  = 1000
	ExportMax = 200_000
)

// ErrDisabled SQLite 存储未开启。
var ErrDisabled = errors.New("SQLite 存储未开启")

const schema = `
CREATE TABLE IF NOT EXISTS requests (
  id INTEGER PRIMARY KEY,
  ts INTEGER NOT NULL,
  request_id TEXT NOT NULL DEFAULT '',
  path TEXT NOT NULL DEFAULT '',
  account TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  status INTEGER NOT NULL DEFAULT 0,
  ok INTEGER NOT NULL DEFAULT 0,
  outcome TEXT NOT NULL DEFAULT '',
  duration_ms INTEGER NOT NULL DEFAULT 0,
  ttfb_ms INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  credit REAL NOT NULL DEFAULT 0,
  credit_known INTEGER NOT NULL DEFAULT 0,
  cache_hit_tokens INTEGER NOT NULL DEFAULT 0,
  cache_miss_tokens INTEGER NOT NULL DEFAULT 0,
  client_ip TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_requests_ts ON requests(ts);
CREATE INDEX IF NOT EXISTS idx_requests_outcome ON requests(outcome);
CREATE TABLE IF NOT EXISTS logs (
  id INTEGER PRIMARY KEY,
  ts INTEGER NOT NULL,
  ch TEXT NOT NULL,
  level TEXT NOT NULL,
  text TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_logs_ts ON logs(ts);
CREATE INDEX IF NOT EXISTS idx_logs_ch ON logs(ch);
`

const reqCols = "ts, request_id, path, account, model, status, ok, outcome, duration_ms, ttfb_ms, attempts, " +
	"prompt_tokens, completion_tokens, total_tokens, credit, credit_known, cache_hit_tokens, cache_miss_tokens, client_ip, user_agent"

// Settings 存储开关与保留上限（logging.sqlite_*）。
type Settings struct {
	Enabled       bool
	RetentionDays int
	MaxRows       int
	MaxMB         int
}

// LogRow 一条运行日志。Level：e=错误 / w=警告 / 空=普通（与面板同一组关键词推断）。
type LogRow struct {
	ID    int64     `json:"id"`
	TS    time.Time `json:"ts"`
	Ch    string    `json:"ch"`
	Level string    `json:"level,omitempty"`
	Text  string    `json:"text"`
}

// Stats 面板展示的存储状态。
type Stats struct {
	Enabled       bool      `json:"enabled"`
	Path          string    `json:"path"`
	Bytes         int64     `json:"bytes"`
	Requests      int64     `json:"requests"`
	Logs          int64     `json:"logs"`
	OldestRequest time.Time `json:"oldest_request,omitzero"`
	OldestLog     time.Time `json:"oldest_log,omitzero"`
	RetentionDays int       `json:"retention_days"`
	MaxRows       int       `json:"max_rows"`
	MaxMB         int       `json:"max_mb"`
	Dropped       uint64    `json:"dropped"`
	LastError     string    `json:"last_error,omitempty"`
}

type item struct {
	req *reqlog.Event
	log *LogRow
}

// Store SQLite 存储。零值不可用，用 New 创建；Configure 决定是否真正打开数据库。
type Store struct {
	path     string
	mu       sync.Mutex // 串行化 Configure/Close
	w        atomic.Pointer[worker]
	settings atomic.Pointer[Settings]
	dropped  atomic.Uint64
	lastErr  atomic.Pointer[string]
}

type worker struct {
	db        *sql.DB
	lastBytes time.Time // 上次检查容量上限的时间
	ch        chan item
	stop      chan struct{}
	done      chan struct{}
}

// New 创建存储（不打开文件）。path 为数据库文件路径。
func New(path string) *Store {
	s := &Store{path: path}
	s.settings.Store(&Settings{})
	return s
}

// Path 数据库文件路径。
func (s *Store) Path() string { return s.path }

// Enabled 是否已开启。
func (s *Store) Enabled() bool { return s != nil && s.w.Load() != nil }

// Configure 应用设置：开关翻转时打开或关闭数据库；上限变化立即用于下一轮清理。
// 关闭不删除文件，重新开启接着写。
func (s *Store) Configure(st Settings) error {
	if s == nil {
		return nil
	}
	if st.RetentionDays < 0 {
		st.RetentionDays = 0
	}
	if st.MaxRows < 0 {
		st.MaxRows = 0
	}
	if st.MaxMB < 0 {
		st.MaxMB = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings.Store(&st)
	cur := s.w.Load()
	switch {
	case st.Enabled && cur == nil:
		w, err := s.open()
		if err != nil {
			s.setErr(err)
			return err
		}
		// 打开时先同步清理一轮：重启后看到的就是已按上限裁剪过的数据。
		s.trim(w)
		s.w.Store(w)
		go s.run(w)
	case !st.Enabled && cur != nil:
		s.w.Store(nil)
		cur.shutdown()
	}
	return nil
}

// Close 刷完队列并关闭数据库（幂等）。
func (s *Store) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.w.Swap(nil); w != nil {
		w.shutdown()
	}
}

func (w *worker) shutdown() {
	close(w.stop)
	<-w.done
}

func (s *Store) open() (*worker, error) {
	if dir := filepath.Dir(s.path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	// 回滚日志（不用 WAL）：容量上限针对的是用户看得到的那一个文件，WAL 会把最新
	// 的写入停在一个上限管不到的旁路文件里。
	dsn := "file:" + filepath.ToSlash(s.path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(DELETE)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// 单连接：写入、清理、查询、导出全部串行，没有读写锁竞争要处理。
	db.SetMaxOpenConns(1)
	// auto_vacuum 只有在建第一张表之前设置才生效；老库上是空操作，容量上限退回整库 VACUUM。
	if _, err := db.Exec("PRAGMA auto_vacuum=INCREMENTAL"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化 SQLite 表结构: %w", err)
	}
	_ = os.Chmod(s.path, 0o600)
	return &worker{db: db, ch: make(chan item, queueSize), stop: make(chan struct{}), done: make(chan struct{})}, nil
}

// ---------------------------------------------------------------- 写入 ----

// AppendRequest 投递一条请求记录；未开启或队列满时立即返回。
func (s *Store) AppendRequest(e reqlog.Event) {
	if s == nil {
		return
	}
	s.enqueue(item{req: &e})
}

// AppendLog 投递一条运行日志；未开启或队列满时立即返回。
func (s *Store) AppendLog(ts time.Time, ch, text string) {
	if s == nil || text == "" {
		return
	}
	if len(text) > textMax {
		text = text[:textMax]
	}
	s.enqueue(item{log: &LogRow{TS: ts, Ch: ch, Level: LogLevel(text), Text: text}})
}

func (s *Store) enqueue(it item) {
	w := s.w.Load()
	if w == nil {
		return
	}
	select {
	case w.ch <- it:
	default:
		s.dropped.Add(1)
	}
}

// LogLevel 按正文关键词推断级别（与面板 logLineHtml 同口径）。
func LogLevel(text string) string {
	switch {
	case strings.Contains(text, "error") || strings.Contains(text, "失败") || strings.Contains(text, "错误"):
		return "e"
	case strings.Contains(text, "warn") || strings.Contains(text, "冷却") || strings.Contains(text, "熔断"):
		return "w"
	}
	return ""
}

func (s *Store) run(w *worker) {
	defer close(w.done)
	defer w.db.Close()
	tick := time.NewTicker(trimInterval)
	defer tick.Stop()
	for {
		select {
		case it := <-w.ch:
			batch := []item{it}
			for len(batch) < batchMax {
				select {
				case nx := <-w.ch:
					batch = append(batch, nx)
					continue
				default:
				}
				break
			}
			s.write(w.db, batch)
		case <-tick.C:
			s.trim(w)
		case <-w.stop:
			var batch []item
			for {
				select {
				case it := <-w.ch:
					batch = append(batch, it)
					continue
				default:
				}
				break
			}
			if len(batch) > 0 {
				s.write(w.db, batch)
			}
			return
		}
	}
}

func (s *Store) write(db *sql.DB, batch []item) {
	tx, err := db.Begin()
	if err != nil {
		s.setErr(err)
		return
	}
	reqStmt, err := tx.Prepare("INSERT INTO requests (" + reqCols + ") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
	if err != nil {
		_ = tx.Rollback()
		s.setErr(err)
		return
	}
	logStmt, err := tx.Prepare("INSERT INTO logs (ts, ch, level, text) VALUES (?,?,?,?)")
	if err != nil {
		_ = tx.Rollback()
		s.setErr(err)
		return
	}
	for _, it := range batch {
		if e := it.req; e != nil {
			_, err = reqStmt.Exec(e.Time.UnixMilli(), e.RequestID, e.Path, e.Account, e.Model, e.Status, b2i(e.OK),
				e.Outcome, e.DurationMs, e.TTFBMs, e.Attempts, e.PromptTokens, e.CompletionTokens, e.TotalTokens,
				e.Credit, b2i(e.HasCredit), e.CacheHitTokens, e.CacheMissTokens, e.ClientIP, e.UserAgent)
		} else if l := it.log; l != nil {
			_, err = logStmt.Exec(l.TS.UnixMilli(), l.Ch, l.Level, l.Text)
		}
		if err != nil {
			s.setErr(err)
		}
	}
	if err := tx.Commit(); err != nil {
		s.setErr(err)
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- 清理 ----

func (s *Store) trim(w *worker) {
	db := w.db
	st := *s.settings.Load()
	if st.RetentionDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -st.RetentionDays).UnixMilli()
		s.exec(db, "DELETE FROM requests WHERE ts < ?", cutoff)
		s.exec(db, "DELETE FROM logs WHERE ts < ?", cutoff)
	}
	if st.MaxRows > 0 {
		for _, t := range []string{"requests", "logs"} {
			s.exec(db, "DELETE FROM "+t+" WHERE id <= (SELECT id FROM "+t+" ORDER BY id DESC LIMIT 1 OFFSET ?)", st.MaxRows)
		}
	}
	if st.MaxMB > 0 && time.Since(w.lastBytes) >= bytesInterval {
		w.lastBytes = time.Now()
		s.trimBytes(db, int64(st.MaxMB)<<20)
	}
}

// trimBytes 文件超过 cap 时按平均行宽估算要删的比例，两张表各删最旧的那部分，
// 然后 VACUUM 收缩；估少了再来一轮，最多 4 轮。目标压到 85%，免得下一批写入立刻再超。
func (s *Store) trimBytes(db *sql.DB, cap int64) {
	for range 4 {
		size := s.fileSize()
		if size <= cap {
			return
		}
		var nReq, nLog int64
		_ = db.QueryRow("SELECT COUNT(*) FROM requests").Scan(&nReq)
		_ = db.QueryRow("SELECT COUNT(*) FROM logs").Scan(&nLog)
		if nReq+nLog == 0 {
			return
		}
		keep := float64(cap) * 0.85 / float64(size)
		for t, n := range map[string]int64{"requests": nReq, "logs": nLog} {
			if drop := n - int64(float64(n)*keep); drop > 0 {
				s.exec(db, "DELETE FROM "+t+" WHERE id IN (SELECT id FROM "+t+" ORDER BY id ASC LIMIT ?)", drop)
			}
		}
		s.exec(db, "VACUUM")
		if s.fileSize() >= size {
			return
		}
	}
}

func (s *Store) exec(db *sql.DB, q string, args ...any) {
	if _, err := db.Exec(q, args...); err != nil {
		s.setErr(err)
	}
}

func (s *Store) fileSize() int64 {
	var total int64
	for _, suf := range []string{"", "-journal", "-wal"} {
		if st, err := os.Stat(s.path + suf); err == nil {
			total += st.Size()
		}
	}
	return total
}

func (s *Store) setErr(err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	s.lastErr.Store(&msg)
}

// ---------------------------------------------------------------- 查询 ----

// RequestQuery 请求记录检索条件。Q 按空白分词，每个词都要命中（请求 ID / 账号 /
// 模型 / IP / UA / 路径，大小写不敏感的包含匹配）；From/To 闭区间，零值不设界。
type RequestQuery struct {
	Q       string
	Outcome string
	From    time.Time
	To      time.Time
	Limit   int
	Offset  int
}

// LogQuery 运行日志检索条件。Ch / Level 精确匹配，Q 同上（只匹配正文）。
type LogQuery struct {
	Q      string
	Ch     string
	Level  string
	From   time.Time
	To     time.Time
	Limit  int
	Offset int
}

func likeArg(kw string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(strings.ToLower(kw)) + "%"
}

func timeWhere(where []string, args []any, from, to time.Time) ([]string, []any) {
	if !from.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, from.UnixMilli())
	}
	if !to.IsZero() {
		where = append(where, "ts <= ?")
		args = append(args, to.UnixMilli())
	}
	return where, args
}

func (q RequestQuery) where() (string, []any) {
	var where []string
	var args []any
	if q.Outcome != "" {
		where = append(where, "outcome = ?")
		args = append(args, q.Outcome)
	}
	where, args = timeWhere(where, args, q.From, q.To)
	for _, kw := range strings.Fields(q.Q) {
		where = append(where, `lower(request_id || ' ' || account || ' ' || model || ' ' || client_ip || ' ' || user_agent || ' ' || path) LIKE ? ESCAPE '\'`)
		args = append(args, likeArg(kw))
	}
	return joinWhere(where), args
}

func (q LogQuery) where() (string, []any) {
	var where []string
	var args []any
	if q.Ch != "" {
		where = append(where, "ch = ?")
		args = append(args, q.Ch)
	}
	if q.Level != "" {
		where = append(where, "level = ?")
		args = append(args, q.Level)
	}
	where, args = timeWhere(where, args, q.From, q.To)
	for _, kw := range strings.Fields(q.Q) {
		where = append(where, `lower(text) LIKE ? ESCAPE '\'`)
		args = append(args, likeArg(kw))
	}
	return joinWhere(where), args
}

func joinWhere(where []string) string {
	if len(where) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(where, " AND ")
}

func clampPage(limit, offset, max int) (int, int) {
	if limit <= 0 {
		limit = 200
	}
	if limit > max {
		limit = max
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func (s *Store) db() (*sql.DB, error) {
	if s == nil {
		return nil, ErrDisabled
	}
	w := s.w.Load()
	if w == nil {
		return nil, ErrDisabled
	}
	return w.db, nil
}

// Requests 按时间倒序检索请求记录，返回本页与命中总数。
func (s *Store) Requests(ctx context.Context, q RequestQuery) ([]reqlog.Event, int64, error) {
	db, err := s.db()
	if err != nil {
		return nil, 0, err
	}
	where, args := q.where()
	var total int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM requests"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit, offset := clampPage(q.Limit, q.Offset, QueryMax)
	out := []reqlog.Event{}
	err = s.eachRequest(ctx, db, where, args, limit, offset, func(e reqlog.Event) error {
		out = append(out, e)
		return nil
	})
	return out, total, err
}

func (s *Store) eachRequest(ctx context.Context, db *sql.DB, where string, args []any, limit, offset int, fn func(reqlog.Event) error) error {
	rows, err := db.QueryContext(ctx, "SELECT "+reqCols+" FROM requests"+where+" ORDER BY id DESC LIMIT ? OFFSET ?",
		append(args, limit, offset)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var e reqlog.Event
		var ts int64
		var ok, known int
		if err := rows.Scan(&ts, &e.RequestID, &e.Path, &e.Account, &e.Model, &e.Status, &ok, &e.Outcome,
			&e.DurationMs, &e.TTFBMs, &e.Attempts, &e.PromptTokens, &e.CompletionTokens, &e.TotalTokens,
			&e.Credit, &known, &e.CacheHitTokens, &e.CacheMissTokens, &e.ClientIP, &e.UserAgent); err != nil {
			return err
		}
		e.Time = time.UnixMilli(ts)
		e.OK, e.HasCredit = ok != 0, known != 0
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Logs 按时间倒序检索运行日志，返回本页与命中总数。
func (s *Store) Logs(ctx context.Context, q LogQuery) ([]LogRow, int64, error) {
	db, err := s.db()
	if err != nil {
		return nil, 0, err
	}
	where, args := q.where()
	var total int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM logs"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit, offset := clampPage(q.Limit, q.Offset, QueryMax)
	out := []LogRow{}
	err = s.eachLog(ctx, db, where, args, limit, offset, func(l LogRow) error {
		out = append(out, l)
		return nil
	})
	return out, total, err
}

func (s *Store) eachLog(ctx context.Context, db *sql.DB, where string, args []any, limit, offset int, fn func(LogRow) error) error {
	rows, err := db.QueryContext(ctx, "SELECT id, ts, ch, level, text FROM logs"+where+" ORDER BY id DESC LIMIT ? OFFSET ?",
		append(args, limit, offset)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var l LogRow
		var ts int64
		if err := rows.Scan(&l.ID, &ts, &l.Ch, &l.Level, &l.Text); err != nil {
			return err
		}
		l.TS = time.UnixMilli(ts)
		if err := fn(l); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ---------------------------------------------------------------- 导出 ----

const exportTime = "2006-01-02 15:04:05"

var reqHeader = []string{"time", "request_id", "path", "account", "model", "status", "ok", "outcome",
	"duration_ms", "ttfb_ms", "attempts", "prompt_tokens", "completion_tokens", "total_tokens",
	"credit", "credit_known", "cache_hit_tokens", "cache_miss_tokens", "client_ip", "user_agent"}

// ExportRequests 按检索条件导出请求记录（新的在前，最多 ExportMax 条）。
// format：csv（带 UTF-8 BOM，Excel 直接打开不乱码）或 jsonl。返回导出条数。
func (s *Store) ExportRequests(ctx context.Context, out io.Writer, q RequestQuery, format string) (int, error) {
	db, err := s.db()
	if err != nil {
		return 0, err
	}
	where, args := q.where()
	bw := bufio.NewWriterSize(out, 64<<10)
	n := 0
	var write func(reqlog.Event) error
	var cw *csv.Writer
	if format == "jsonl" {
		enc := json.NewEncoder(bw)
		write = func(e reqlog.Event) error { n++; return enc.Encode(e) }
	} else {
		_, _ = bw.WriteString("\ufeff")
		cw = csv.NewWriter(bw)
		_ = cw.Write(reqHeader)
		write = func(e reqlog.Event) error {
			n++
			return cw.Write([]string{e.Time.Format(exportTime), e.RequestID, e.Path, e.Account, e.Model,
				strconv.Itoa(e.Status), strconv.FormatBool(e.OK), e.Outcome,
				i64(e.DurationMs), i64(e.TTFBMs), strconv.Itoa(e.Attempts),
				i64(e.PromptTokens), i64(e.CompletionTokens), i64(e.TotalTokens),
				strconv.FormatFloat(e.Credit, 'f', -1, 64), strconv.FormatBool(e.HasCredit),
				i64(e.CacheHitTokens), i64(e.CacheMissTokens), e.ClientIP, e.UserAgent})
		}
	}
	err = s.eachRequest(ctx, db, where, args, ExportMax, 0, write)
	if cw != nil {
		cw.Flush()
		if err == nil {
			err = cw.Error()
		}
	}
	if ferr := bw.Flush(); err == nil {
		err = ferr
	}
	return n, err
}

// ExportLogs 按检索条件导出运行日志。csv 列：time, channel, level, text；
// jsonl 每行一个 LogRow。
func (s *Store) ExportLogs(ctx context.Context, out io.Writer, q LogQuery, format string) (int, error) {
	db, err := s.db()
	if err != nil {
		return 0, err
	}
	where, args := q.where()
	bw := bufio.NewWriterSize(out, 64<<10)
	n := 0
	var write func(LogRow) error
	var cw *csv.Writer
	if format == "jsonl" {
		enc := json.NewEncoder(bw)
		write = func(l LogRow) error { n++; return enc.Encode(l) }
	} else {
		_, _ = bw.WriteString("\ufeff")
		cw = csv.NewWriter(bw)
		_ = cw.Write([]string{"time", "channel", "level", "text"})
		write = func(l LogRow) error {
			n++
			return cw.Write([]string{l.TS.Format(exportTime), l.Ch, l.Level, l.Text})
		}
	}
	err = s.eachLog(ctx, db, where, args, ExportMax, 0, write)
	if cw != nil {
		cw.Flush()
		if err == nil {
			err = cw.Error()
		}
	}
	if ferr := bw.Flush(); err == nil {
		err = ferr
	}
	return n, err
}

func i64(v int64) string { return strconv.FormatInt(v, 10) }

// ---------------------------------------------------------------- 状态 ----

// Stats 当前存储状态（未开启时只有 Path/上限/Enabled=false）。
func (s *Store) Stats(ctx context.Context) Stats {
	st := *s.settings.Load()
	out := Stats{Path: s.path, RetentionDays: st.RetentionDays, MaxRows: st.MaxRows, MaxMB: st.MaxMB,
		Dropped: s.dropped.Load(), Bytes: s.fileSize()}
	if p := s.lastErr.Load(); p != nil {
		out.LastError = *p
	}
	db, err := s.db()
	if err != nil {
		return out
	}
	out.Enabled = true
	var minReq, minLog sql.NullInt64
	_ = db.QueryRowContext(ctx, "SELECT COUNT(*), MIN(ts) FROM requests").Scan(&out.Requests, &minReq)
	_ = db.QueryRowContext(ctx, "SELECT COUNT(*), MIN(ts) FROM logs").Scan(&out.Logs, &minLog)
	if minReq.Valid {
		out.OldestRequest = time.UnixMilli(minReq.Int64)
	}
	if minLog.Valid {
		out.OldestLog = time.UnixMilli(minLog.Int64)
	}
	return out
}

// Clear 清空一张表（kind = requests / logs / all）并收缩文件。
func (s *Store) Clear(ctx context.Context, kind string) error {
	db, err := s.db()
	if err != nil {
		return err
	}
	var tables []string
	switch kind {
	case "requests", "logs":
		tables = []string{kind}
	case "all":
		tables = []string{"requests", "logs"}
	default:
		return fmt.Errorf("未知的清空范围 %q", kind)
	}
	for _, t := range tables {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return err
		}
	}
	_, err = db.ExecContext(ctx, "VACUUM")
	return err
}
