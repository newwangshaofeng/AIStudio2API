package requestdb

import (
	"context"
	"encoding/json"
	"time"
	_ "time/tzdata"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// quotaTimezone 是 Google AI Studio 每日配额重置使用的时区
const quotaTimezone = "America/Los_Angeles"

// quotaPeakWindow 是推算每日参考上限时统计的历史窗口
const quotaPeakWindow = 30 * 24 * time.Hour

// quotaChannels 是账户页分别展示的上游通道
var quotaChannels = []string{"playground", "build", "assistant"}

// quotaWindow 返回包含 now 的太平洋时间自然日的 UTC 起止时刻
func quotaWindow(now time.Time) (time.Time, time.Time) {
	location, err := time.LoadLocation(quotaTimezone)
	if err != nil {
		location = time.FixedZone("PST", -8*60*60)
	}
	local := now.In(location)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	return start.UTC(), start.AddDate(0, 0, 1).UTC()
}

// AccountQuota 汇总太平洋时间当日内各账户按通道划分的请求与 token 用量，并给出最近 30 天单日峰值作为参考上限
func (store *Store) AccountQuota(ctx context.Context, now time.Time) (api.AccountQuotaReport, error) {
	windowStart, resetAt := quotaWindow(now)
	report := api.AccountQuotaReport{
		WindowStart: windowStart, ResetAt: resetAt, Timezone: quotaTimezone,
		GeneratedAt: now.UTC(), Accounts: map[string]api.AccountQuotaSummary{}, Peaks: map[string]int64{},
	}
	if err := store.scanQuotaUsage(ctx, windowStart.UnixMilli(), now.UnixMilli(), report.Accounts); err != nil {
		return api.AccountQuotaReport{}, err
	}
	peaks, err := store.quotaPeaks(ctx, now)
	if err != nil {
		return api.AccountQuotaReport{}, err
	}
	report.Peaks = peaks
	return report, nil
}

// scanQuotaUsage 从原始记录汇总当日各账户通道用量，失败尝试按通道计入拒绝次数
func (store *Store) scanQuotaUsage(ctx context.Context, from, to int64, accounts map[string]api.AccountQuotaSummary) error {
	rows, err := store.db.QueryContext(ctx, `SELECT time, account, channel, status, state, tool_calls,
 input_tokens, cached_tokens, reasoning_tokens, reply_tokens, total_tokens, attempts
 FROM requests WHERE time >= ? AND time < ?`, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var at int64
		var account, channel, state, attempts string
		var status, toolCalls int
		var inputTokens, cachedTokens, reasoningTokens, replyTokens, totalTokens int64
		if err := rows.Scan(&at, &account, &channel, &status, &state, &toolCalls,
			&inputTokens, &cachedTokens, &reasoningTokens, &replyTokens, &totalTokens, &attempts); err != nil {
			return err
		}
		if account == "" || channel == "" {
			continue
		}
		summary := accounts[account]
		if summary.Channels == nil {
			summary = api.AccountQuotaSummary{Account: account, Channels: map[string]api.AccountChannelStats{}}
		}
		current := summary.Channels[channel]
		current.Channel = channel
		current.Requests++
		current.ToolCalls += int64(toolCalls)
		current.InputTokens += inputTokens
		current.CachedTokens += cachedTokens
		current.ReasoningTokens += reasoningTokens
		current.ReplyTokens += replyTokens
		current.TotalTokens += totalTokens
		switch state {
		case "failed":
			current.Failed++
		case "cancelled":
			current.Canceled++
		default:
			current.Succeeded++
		}
		if status == 429 {
			current.RateLimited++
		}
		current.Rejected += quotaRejectedAttempts(attempts, channel)
		if current.LastAt == nil || time.UnixMilli(at).After(*current.LastAt) {
			last := time.UnixMilli(at).UTC()
			current.LastAt = &last
		}
		summary.Channels[channel] = current
		accounts[account] = summary
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for account, summary := range accounts {
		for _, channel := range quotaChannels {
			if _, exists := summary.Channels[channel]; !exists {
				summary.Channels[channel] = api.AccountChannelStats{Channel: channel}
			}
		}
		accounts[account] = summary
	}
	return nil
}

// quotaRejectedAttempts 统计记录中该通道被上游拒绝的尝试次数，用于呈现 Build 无配额等即时拒绝
func quotaRejectedAttempts(encoded string, channel string) int64 {
	if encoded == "" || encoded == "[]" || encoded == "null" {
		return 0
	}
	var attempts []api.RequestAttempt
	if err := json.Unmarshal([]byte(encoded), &attempts); err != nil {
		return 0
	}
	var rejected int64
	for _, attempt := range attempts {
		if attempt.Channel == channel {
			rejected++
		}
	}
	return rejected
}

// quotaDayKey 标识一个账户在某通道某个太平洋本地日的用量
type quotaDayKey struct {
	channel string
	account string
	day     string
}

// quotaPeaks 返回最近 30 天各通道单账户单日请求数峰值，作为账户页进度条的参考上限
func (store *Store) quotaPeaks(ctx context.Context, now time.Time) (map[string]int64, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT time, account, channel FROM requests WHERE time >= ? AND time < ?`,
		now.Add(-quotaPeakWindow).UnixMilli(), now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[quotaDayKey]int64)
	for rows.Next() {
		var at int64
		var account, channel string
		if err := rows.Scan(&at, &account, &channel); err != nil {
			return nil, err
		}
		if account == "" || channel == "" {
			continue
		}
		day, _ := quotaWindow(time.UnixMilli(at))
		counts[quotaDayKey{channel: channel, account: account, day: day.Format(time.DateOnly)}]++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	peaks := make(map[string]int64, len(quotaChannels))
	for key, count := range counts {
		if count > peaks[key.channel] {
			peaks[key.channel] = count
		}
	}
	return peaks, nil
}
