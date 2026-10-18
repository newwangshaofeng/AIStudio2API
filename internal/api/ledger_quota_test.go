package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// quotaLedger 提供固定的账户额度报告并记录调用时间
type quotaLedger struct {
	RequestLedger
	now time.Time
}

// AccountQuota 返回含一个账户三个通道的固定报告
func (ledger *quotaLedger) AccountQuota(_ context.Context, now time.Time) (AccountQuotaReport, error) {
	ledger.now = now
	last := now.UTC()
	return AccountQuotaReport{
		WindowStart: now.UTC().Add(-time.Hour), ResetAt: now.UTC().Add(6 * time.Hour),
		Timezone: "America/Los_Angeles", GeneratedAt: now.UTC(),
		Accounts: map[string]AccountQuotaSummary{
			"a@example.com": {Account: "a@example.com", Channels: map[string]AccountChannelStats{
				"playground": {Channel: "playground", Requests: 3, Succeeded: 2, RateLimited: 1, TotalTokens: 120, LastAt: &last},
				"build":      {Channel: "build", Requests: 1, Rejected: 1},
			}},
		},
		Peaks: map[string]int64{"playground": 3},
	}, nil
}

// TestAccountQuotaEndpoint 验证额度接口在回环地址可读，且 JSON 字段与前端类型一致
func TestAccountQuotaEndpoint(t *testing.T) {
	handler := NewHandler(nil, Config{Ledger: &quotaLedger{}})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, loopbackRequest("GET", "/api/accounts/quota"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d %s", recorder.Code, recorder.Body)
	}
	var body struct {
		WindowStart time.Time `json:"window_start"`
		ResetAt     time.Time `json:"reset_at"`
		Timezone    string    `json:"timezone"`
		Accounts    map[string]struct {
			Account  string `json:"account"`
			Channels map[string]struct {
				Channel     string `json:"channel"`
				Requests    int64  `json:"requests"`
				Rejected    int64  `json:"rejected"`
				RateLimited int64  `json:"rate_limited"`
				TotalTokens int64  `json:"total_tokens"`
			} `json:"channels"`
		} `json:"accounts"`
		Peaks map[string]int64 `json:"peaks"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	account, ok := body.Accounts["a@example.com"]
	if !ok || body.Timezone != "America/Los_Angeles" || body.Peaks["playground"] != 3 {
		t.Fatalf("响应 = %+v", body)
	}
	playground, ok := account.Channels["playground"]
	if !ok || playground.Requests != 3 || playground.RateLimited != 1 || playground.TotalTokens != 120 {
		t.Fatalf("playground = %+v", playground)
	}
	if build := account.Channels["build"]; build.Rejected != 1 {
		t.Fatalf("build = %+v", build)
	}
}

// TestAccountQuotaError 验证账本查询失败时返回用量错误码，而非空报告
func TestAccountQuotaError(t *testing.T) {
	handler := NewHandler(nil, Config{Ledger: &failingQuotaLedger{}})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, loopbackRequest("GET", "/api/accounts/quota"))
	if recorder.Code == http.StatusOK || !strings.Contains(recorder.Body.String(), "usage_failed") {
		t.Fatalf("状态码 = %d %s", recorder.Code, recorder.Body)
	}
}

// loopbackRequest 构造来自回环地址的管理接口请求，控制面只对该来源开放
func loopbackRequest(method, path string) *http.Request {
	request := httptest.NewRequest(method, path, nil)
	request.RemoteAddr = "127.0.0.1:54321"
	request.Host = "127.0.0.1:2048"
	return request
}

// failingQuotaLedger 让额度查询返回错误
type failingQuotaLedger struct {
	RequestLedger
}

// AccountQuota 返回固定的查询错误
func (ledger *failingQuotaLedger) AccountQuota(context.Context, time.Time) (AccountQuotaReport, error) {
	return AccountQuotaReport{}, io.ErrUnexpectedEOF
}
