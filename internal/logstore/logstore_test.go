package logstore

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// flush 关闭再重开，确保队列里的记录都已提交。
func flush(t *testing.T, s *Store, st Settings) {
	t.Helper()
	s.Close()
	if err := s.Configure(st); err != nil {
		t.Fatal(err)
	}
}

func newStore(t *testing.T, st Settings) *Store {
	t.Helper()
	s := New(filepath.Join(t.TempDir(), "logs.sqlite3"))
	st.Enabled = true
	if err := s.Configure(st); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestDisabledIsNoop(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "x.sqlite3"))
	s.AppendRequest(reqlog.Event{RequestID: "r"})
	s.AppendLog(time.Now(), "sys", "hi")
	if _, _, err := s.Requests(context.Background(), RequestQuery{}); err != ErrDisabled {
		t.Fatalf("未开启应返回 ErrDisabled: %v", err)
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Fatal("未开启不应创建数据库文件")
	}
	var nilStore *Store
	nilStore.AppendRequest(reqlog.Event{})
	nilStore.Close()
}

func TestSearchAndFilters(t *testing.T) {
	s := newStore(t, Settings{})
	base := time.Now().Add(-time.Hour)
	s.AppendRequest(reqlog.Event{Time: base, RequestID: "req-a", Model: "glm-5.2", Account: "阿猫(1234abcd)", Outcome: "success", Status: 200, OK: true, ClientIP: "10.0.0.1", Credit: 1.5, HasCredit: true})
	s.AppendRequest(reqlog.Event{Time: base.Add(time.Minute), RequestID: "req-b", Model: "kimi-k3", Account: "阿狗", Outcome: "http_error", Status: 429, UserAgent: "curl/8"})
	s.AppendRequest(reqlog.Event{Time: base.Add(2 * time.Minute), RequestID: "req_c%", Model: "glm-5.2", Outcome: "success", Status: 200, OK: true})
	s.AppendLog(base, "chat", "| #1 glm-5.2 ok")
	s.AppendLog(base.Add(time.Minute), "sys", "[upstream] 刷新失败: timeout")
	flush(t, s, Settings{Enabled: true})
	ctx := context.Background()

	rows, total, err := s.Requests(ctx, RequestQuery{Q: "GLM"})
	if err != nil || total != 2 || rows[0].RequestID != "req_c%" {
		t.Fatalf("模型搜索 got total=%d rows=%+v err=%v", total, rows, err)
	}
	if rows[1].Credit != 1.5 || !rows[1].HasCredit || !rows[1].OK || rows[1].ClientIP != "10.0.0.1" {
		t.Fatalf("字段往返丢失: %+v", rows[1])
	}
	if _, total, _ = s.Requests(ctx, RequestQuery{Q: "glm 10.0"}); total != 1 {
		t.Fatalf("多词应 AND: %d", total)
	}
	if _, total, _ = s.Requests(ctx, RequestQuery{Outcome: "http_error"}); total != 1 {
		t.Fatalf("outcome 过滤: %d", total)
	}
	if _, total, _ = s.Requests(ctx, RequestQuery{Q: "%"}); total != 1 {
		t.Fatalf("%% 应按字面匹配: %d", total)
	}
	if _, total, _ = s.Requests(ctx, RequestQuery{From: base.Add(30 * time.Second)}); total != 2 {
		t.Fatalf("时间下界: %d", total)
	}
	page, total, _ := s.Requests(ctx, RequestQuery{Limit: 1, Offset: 1})
	if total != 3 || len(page) != 1 || page[0].RequestID != "req-b" {
		t.Fatalf("分页: %+v", page)
	}

	logs, total, _ := s.Logs(ctx, LogQuery{Level: "e"})
	if total != 1 || logs[0].Ch != "sys" || !strings.Contains(logs[0].Text, "刷新失败") {
		t.Fatalf("日志级别过滤: %+v", logs)
	}
	if _, total, _ = s.Logs(ctx, LogQuery{Ch: "chat", Q: "GLM"}); total != 1 {
		t.Fatalf("日志频道+搜索: %d", total)
	}
}

func TestExportFormats(t *testing.T) {
	s := newStore(t, Settings{})
	s.AppendRequest(reqlog.Event{Time: time.Now(), RequestID: "r1", Model: "m,with,comma", Outcome: "success"})
	s.AppendLog(time.Now(), "sys", "line \"quoted\"")
	flush(t, s, Settings{Enabled: true})
	ctx := context.Background()

	var buf bytes.Buffer
	n, err := s.ExportRequests(ctx, &buf, RequestQuery{}, "csv")
	out := buf.String()
	if err != nil || n != 1 || !strings.HasPrefix(out, "\ufefftime,request_id") || !strings.Contains(out, `"m,with,comma"`) {
		t.Fatalf("csv n=%d err=%v out=%q", n, err, out)
	}
	buf.Reset()
	if n, err = s.ExportLogs(ctx, &buf, LogQuery{}, "jsonl"); err != nil || n != 1 {
		t.Fatalf("jsonl n=%d err=%v", n, err)
	}
	var row LogRow
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &row); err != nil || row.Text != `line "quoted"` {
		t.Fatalf("jsonl 行: %v %+v", err, row)
	}
}

func TestRetentionRowsAndClear(t *testing.T) {
	st := Settings{Enabled: true, RetentionDays: 1, MaxRows: 3}
	s := newStore(t, st)
	s.AppendRequest(reqlog.Event{Time: time.Now().AddDate(0, 0, -3), RequestID: "old"})
	for i := range 5 {
		s.AppendRequest(reqlog.Event{Time: time.Now(), RequestID: string(rune('a' + i))})
	}
	flush(t, s, st) // 重开时立即执行一轮清理
	ctx := context.Background()
	rows, total, _ := s.Requests(ctx, RequestQuery{})
	if total != 3 || rows[0].RequestID != "e" || rows[2].RequestID != "c" {
		t.Fatalf("应只保留最新 3 条且过期行被删: total=%d %+v", total, rows)
	}
	if err := s.Clear(ctx, "all"); err != nil {
		t.Fatal(err)
	}
	if got := s.Stats(ctx); got.Requests != 0 || !got.Enabled {
		t.Fatalf("清空后 stats=%+v", got)
	}
	if err := s.Clear(ctx, "bogus"); err == nil {
		t.Fatal("未知范围应报错")
	}
}

func TestDisableKeepsFile(t *testing.T) {
	s := newStore(t, Settings{})
	s.AppendRequest(reqlog.Event{Time: time.Now(), RequestID: "keep"})
	if err := s.Configure(Settings{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if s.Enabled() {
		t.Fatal("应已关闭")
	}
	s.AppendRequest(reqlog.Event{Time: time.Now(), RequestID: "dropped-while-off"})
	if err := s.Configure(Settings{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	rows, total, _ := s.Requests(context.Background(), RequestQuery{})
	if total != 1 || rows[0].RequestID != "keep" {
		t.Fatalf("重开后应保留旧数据: %+v", rows)
	}
}
