package requestdb

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// TestLedgerUsageAndBodies 验证关闭前排队的记录落盘、当前与上一周期分开汇总、正文只保留最近条数
func TestLedgerUsageAndBodies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	logs := func(level, message string) { t.Errorf("%s %s", level, message) }
	store, err := Open(path, logs)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	store.Record(Row{ID: "a", Time: now.Add(-time.Hour), Model: "flash", State: "completed", Duration: 2 * time.Second, InputTokens: 10, TotalTokens: 30})
	store.Record(Row{ID: "b", Time: now.Add(-2 * time.Hour), Model: "flash", State: "failed", Status: 429, Duration: 4 * time.Second})
	store.Record(Row{ID: "c", Time: now.Add(-3 * time.Hour), Model: "pro", State: "completed", Duration: 6 * time.Second, TotalTokens: 5})
	store.Record(Row{ID: "old", Time: now.Add(-30 * time.Hour), Model: "pro", State: "completed", Duration: time.Second})
	for index := range bodyRetention + 2 {
		store.SaveBody(api.RequestBody{ID: fmt.Sprintf("body-%d", index), Time: now, Request: "in", Response: "out"})
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path, logs)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	report, err := store.Usage(context.Background(), api.UsageQuery{
		From: now.Add(-24 * time.Hour), To: now, Bucket: time.Hour, Location: time.UTC, Stack: "model",
	})
	if err != nil {
		t.Fatal(err)
	}
	totals := report.Totals
	if totals.Requests != 3 || totals.Failed != 1 || totals.RateLimited != 1 || totals.TotalTokens != 35 || report.Previous.Requests != 1 {
		t.Fatalf("totals=%+v previous=%+v", totals, report.Previous)
	}
	if len(report.Buckets) != 25 || math.Abs(totals.Duration.P95MS-6000) > 60 || totals.Duration.AvgMS != 4000 {
		t.Fatalf("buckets=%d duration=%+v", len(report.Buckets), totals.Duration)
	}
	bucketed := int64(0)
	for _, bucket := range report.Buckets {
		bucketed += bucket.Requests
	}
	models := report.Groups["model"]
	if bucketed != 3 || len(models) != 2 || models[0].Key != "flash" || models[0].Requests != 2 {
		t.Fatalf("bucketed=%d models=%+v", bucketed, models)
	}
	if _, err := store.RequestBody(context.Background(), "body-1"); !errors.Is(err, api.ErrRequestBodyNotFound) {
		t.Fatalf("超出保留条数的正文仍存在: %v", err)
	}
	body, err := store.RequestBody(context.Background(), fmt.Sprintf("body-%d", bodyRetention+1))
	if err != nil || body.Request != "in" || body.Response != "out" {
		t.Fatalf("body=%+v err=%v", body, err)
	}
}

// TestAccountQuota 验证额度按太平洋本地日窗口统计、三通道分组补齐，以及 429 与上游拒绝计数
func TestAccountQuota(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	logs := func(level, message string) { t.Errorf("%s %s", level, message) }
	store, err := Open(path, logs)
	if err != nil {
		t.Fatal(err)
	}
	// 2026-10-18 05:00 UTC 是太平洋时间 2026-10-17 22:00，窗口起点应为 2026-10-17 07:00 UTC
	now := time.Date(2026, 10, 18, 5, 0, 0, 0, time.UTC)
	inWindow := time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC)
	beforeWindow := time.Date(2026, 10, 17, 6, 0, 0, 0, time.UTC)
	rejected := []api.RequestAttempt{{Account: "a@example.com", Channel: "build", Error: "limit: 0"}}
	store.Record(Row{ID: "p1", Time: inWindow, Model: "gemini-4-argon", Account: "a@example.com", Channel: "playground",
		Status: 200, State: "completed", TotalTokens: 100, InputTokens: 60, ReplyTokens: 40})
	store.Record(Row{ID: "p2", Time: inWindow, Model: "gemini-4-argon", Account: "a@example.com", Channel: "playground",
		Status: 429, State: "failed", TotalTokens: 10})
	store.Record(Row{ID: "b1", Time: inWindow, Model: "gemini-3.8-flash", Account: "a@example.com", Channel: "build",
		Status: 200, State: "tool_calls", ToolCalls: 2, TotalTokens: 50, Attempts: rejected})
	store.Record(Row{ID: "s1", Time: inWindow, Model: "gemini-4-argon-build", Account: "a@example.com", Channel: "assistant",
		Status: 200, State: "completed", TotalTokens: 20})
	store.Record(Row{ID: "old", Time: beforeWindow, Model: "gemini-4-argon", Account: "a@example.com", Channel: "playground",
		Status: 200, State: "completed", TotalTokens: 999})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path, logs)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	report, err := store.AccountQuota(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if !report.WindowStart.Equal(time.Date(2026, 10, 17, 7, 0, 0, 0, time.UTC)) ||
		!report.ResetAt.Equal(time.Date(2026, 10, 18, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("window=%s reset=%s", report.WindowStart, report.ResetAt)
	}
	summary, ok := report.Accounts["a@example.com"]
	if !ok {
		t.Fatalf("账户缺失: %+v", report.Accounts)
	}
	if len(summary.Channels) != 3 {
		t.Fatalf("通道未补齐: %+v", summary.Channels)
	}
	playground := summary.Channels["playground"]
	if playground.Requests != 2 || playground.Succeeded != 1 || playground.Failed != 1 ||
		playground.RateLimited != 1 || playground.TotalTokens != 110 {
		t.Fatalf("playground=%+v", playground)
	}
	build := summary.Channels["build"]
	if build.Requests != 1 || build.ToolCalls != 2 || build.Rejected != 1 || build.TotalTokens != 50 {
		t.Fatalf("build=%+v", build)
	}
	if assistant := summary.Channels["assistant"]; assistant.Requests != 1 || assistant.TotalTokens != 20 {
		t.Fatalf("assistant=%+v", assistant)
	}
	// beforeWindow 属于太平洋时间 10-16，因此 10-17 的 Playground 峰值为 2
	if report.Peaks["playground"] != 2 || report.Peaks["build"] != 1 || report.Peaks["assistant"] != 1 {
		t.Fatalf("peaks=%+v", report.Peaks)
	}
}
