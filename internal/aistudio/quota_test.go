package aistudio

import (
	"net/http"
	"testing"
	"time"
)

// quotaError 构造带额度消息的 429 上游错误
func quotaError(message string, retryDelay time.Duration, metadata map[string]string) error {
	return &RPCError{
		Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8,
		Message: message, Metadata: metadata, RetryDelay: retryDelay,
	}
}

// TestPlanQuotaReset 验证套餐限额恢复时刻为下一个太平洋时间 00:05，并覆盖夏令时两侧
func TestPlanQuotaReset(t *testing.T) {
	location, err := time.LoadLocation(quotaResetTimezone)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "夏令时当天中午",
			now:  time.Date(2026, 10, 18, 12, 0, 0, 0, location),
			want: time.Date(2026, 10, 19, 0, 5, 0, 0, location),
		},
		{
			name: "重置后一分钟仍算次日",
			now:  time.Date(2026, 10, 18, 0, 1, 0, 0, location),
			want: time.Date(2026, 10, 19, 0, 5, 0, 0, location),
		},
		{
			name: "冬令时当天深夜",
			now:  time.Date(2026, 12, 24, 23, 30, 0, 0, location),
			want: time.Date(2026, 12, 25, 0, 5, 0, 0, location),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := planQuotaReset(testCase.now)
			if !got.Equal(testCase.want) {
				t.Fatalf("planQuotaReset(%s) = %s，期望 %s", testCase.now, got, testCase.want)
			}
			if buffer := got.Sub(nextQuotaDay(testCase.now)); buffer != planQuotaResetBuffer {
				t.Fatalf("重置缓冲 = %s，期望 %s", buffer, planQuotaResetBuffer)
			}
		})
	}
}

// TestPlanLimitWithoutRetryDelay 验证套餐限额不再递增退避，直接冷却到太平洋时间重置点
func TestPlanLimitWithoutRetryDelay(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 19, 40, 0, time.UTC)
	err := quotaError("You've reached your usage limit. Retry with an alternative Gemini model or "+
		"upgrade your Google AI plan for higher limits. To view plans head to https://one.google.com/ai.", 0, nil)
	cooldown, ok := QuotaCooldownForError(err, now)
	if !ok {
		t.Fatal("套餐限额应产生冷却")
	}
	if cooldown.Kind != "套餐限额" {
		t.Fatalf("类型 = %q，期望 套餐限额", cooldown.Kind)
	}
	if cooldown.Escalate {
		t.Fatal("套餐限额不应再走递增退避，否则 Until 会被 planLimitCooldown 覆盖")
	}
	if want := planQuotaReset(now); !cooldown.Until.Equal(want) {
		t.Fatalf("Until = %s，期望 %s", cooldown.Until, want)
	}
}

// TestPlanLimitKeepsRetryDelay 验证上游给出重试时长时不被日重置点覆盖
func TestPlanLimitKeepsRetryDelay(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 19, 40, 0, time.UTC)
	err := quotaError("You've reached your usage limit.", 21*time.Hour+34*time.Minute, nil)
	cooldown, ok := QuotaCooldownForError(err, now)
	if !ok {
		t.Fatal("429 应产生冷却")
	}
	if want := now.Add(21*time.Hour + 34*time.Minute); !cooldown.Until.Equal(want) {
		t.Fatalf("Until = %s，期望沿用上游重试时长 %s", cooldown.Until, want)
	}
	if cooldown.Kind != "短期限额" {
		t.Fatalf("类型 = %q，期望 短期限额", cooldown.Kind)
	}
}

// TestQuotaCooldownKinds 验证分钟、每日与无配额三类额度限制仍按原规则判定
func TestQuotaCooldownKinds(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	minute := quotaError("Generate requests per minute exceeded",
		time.Minute, map[string]string{"quota_unit": "requests_per_minute"})
	if cooldown, ok := QuotaCooldownForError(minute, now); !ok || cooldown.Kind != "分钟限额" {
		t.Fatalf("分钟限额 = %+v ok=%v", cooldown, ok)
	}
	daily := quotaError("Quota exceeded", 0, map[string]string{"quota_unit": "requests_per_day"})
	if cooldown, ok := QuotaCooldownForError(daily, now); !ok || cooldown.Kind != "每日限额" ||
		!cooldown.Until.Equal(nextQuotaDay(now)) {
		t.Fatalf("每日限额 = %+v ok=%v", cooldown, ok)
	}
	// 上游给出重试时长时优先于日重置点，这是既有约定
	dailyWithDelay := quotaError("Quota exceeded", time.Hour, map[string]string{"quota_unit": "requests_per_day"})
	if cooldown, ok := QuotaCooldownForError(dailyWithDelay, now); !ok ||
		!cooldown.Until.Equal(now.Add(time.Hour)) {
		t.Fatalf("每日限额带重试时长 = %+v ok=%v", cooldown, ok)
	}
	zero := quotaError("* Quota exceeded for metric: generate_requests_per_model_per_user, limit: 0, model: gemini-4-argon",
		21*time.Hour+34*time.Minute, nil)
	if cooldown, ok := QuotaCooldownForError(zero, now); !ok || cooldown.Kind != "无配额" {
		t.Fatalf("无配额 = %+v ok=%v", cooldown, ok)
	}
	if name := nextQuotaDay(now).Location().String(); name != quotaResetTimezone {
		t.Fatalf("重置时区 = %s，期望 %s", name, quotaResetTimezone)
	}
}

// TestPlanLimitCooldownPersisted 验证套餐限额写入运行态后 Until 不被递增退避改写
func TestPlanLimitCooldownPersisted(t *testing.T) {
	pool, accountID := cooldownTestPool(t)
	now := time.Date(2026, 10, 18, 12, 19, 40, 0, time.UTC)
	err := quotaError("You've reached your usage limit.", 0, nil)
	cooldown, ok := QuotaCooldownForError(err, now)
	if !ok {
		t.Fatal("套餐限额应产生冷却")
	}
	generation := pool.ModelAccessGeneration(accountID)
	checkedAt := time.Now()
	until, stateErr := pool.MarkQuotaCooldownIfGeneration(accountID, "gemini-4-argon", generation, checkedAt, cooldown)
	if stateErr != nil {
		t.Fatal(stateErr)
	}
	if !until.Equal(cooldown.Until) {
		t.Fatalf("写入 Until = %s，期望 %s", until, cooldown.Until)
	}
	pool.mu.Lock()
	stored := pool.byID[accountID].runtime.Cooldowns["gemini-4-argon"]
	pool.mu.Unlock()
	if !stored.Until.Equal(cooldown.Until) || stored.Level != 0 {
		t.Fatalf("运行态冷却 = %+v，期望 Until=%s 且不带递增等级", stored, cooldown.Until)
	}
}

// cooldownTestPool 创建含单个就绪账户的账户池
func cooldownTestPool(t *testing.T) (*AccountPool, string) {
	t.Helper()
	pool := &AccountPool{
		byID:     map[string]*Account{},
		accounts: []*Account{},
		changed:  make(chan struct{}),
		lastPicked: map[string]string{},
		lastPickedChannel: map[string]Channel{},
	}
	account := &Account{
		ID: "a@example.com", State: AccountReady,
		Config: AccountConfig{Label: "a@example.com", Enabled: true},
		runtime: accountRuntimeState{
			Cooldowns:   map[string]CooldownState{},
			ModelAccess: map[string]ModelAccess{},
			Resources:   map[string]ResourceBinding{},
		},
	}
	pool.byID[account.ID] = account
	pool.accounts = append(pool.accounts, account)
	return pool, account.ID
}
