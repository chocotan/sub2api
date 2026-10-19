//go:build unit

package service

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestHandle429_ZhipuReset(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	fiveHourReset := now.Add(4 * time.Hour)
	weeklyReset := now.Add(5 * 24 * time.Hour)
	shortReset := now.Add(time.Minute)
	tests := []struct {
		name       string
		headers    http.Header
		body       string
		used5h     any
		usedWeekly any
		reset5h    time.Time
		payg       bool
		wantAt     time.Time
		wantDelay  time.Duration
	}{
		{name: "unknown 429 is transient", body: `{"error":{"message":"Too many requests"}}`, used5h: 20, usedWeekly: 40, wantDelay: 5 * time.Second},
		{name: "reset timestamps alone are not exhaustion", body: `{}`, wantDelay: 5 * time.Second},
		{name: "RPM limit overrides stale exhaustion", body: `{"error":{"code":"1302","message":"Rate limit exceeded"}}`, used5h: 100, usedWeekly: 100, wantDelay: 5 * time.Second},
		{name: "model busy overrides stale exhaustion", body: `{"error":{"code":1305,"message":"Please retry later"}}`, used5h: 100, wantDelay: 5 * time.Second},
		{name: "concurrency limit with retry after", headers: http.Header{"Retry-After": {"60"}}, body: `{"error":{"code":1302,"message":"Concurrent request limit"}}`, used5h: 100, wantDelay: time.Minute},
		{name: "HTTP date retry after", headers: http.Header{"Retry-After": {shortReset.Format(http.TimeFormat)}}, body: `{}`, wantAt: shortReset},
		{name: "body relative reset", body: `{"error":{"type":"rate_limit_exceeded","resets_in_seconds":60}}`, used5h: 100, wantDelay: time.Minute},
		{name: "body absolute reset", body: fmt.Sprintf(`{"error":{"type":"usage_limit_reached","resets_at":%d}}`, shortReset.Unix()), used5h: 100, wantAt: shortReset},
		{name: "retry after takes precedence over body", headers: http.Header{"Retry-After": {"60"}}, body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":7200}}`, wantDelay: time.Minute},
		{name: "invalid retry after allows body reset", headers: http.Header{"Retry-After": {"invalid"}}, body: `{"error":{"type":"rate_limit_exceeded","resets_in_seconds":"60"}}`, wantDelay: time.Minute},
		{name: "negative retry after ignored", headers: http.Header{"Retry-After": {"-60"}}, body: `{}`, wantDelay: 5 * time.Second},
		{name: "zero retry after ignored", headers: http.Header{"Retry-After": {"0"}}, body: `{}`, wantDelay: 5 * time.Second},
		{name: "expired retry after ignored", headers: http.Header{"Retry-After": {now.Add(-time.Minute).Format(http.TimeFormat)}}, body: `{}`, wantDelay: 5 * time.Second},
		{name: "expired body reset ignored", body: `{"error":{"type":"usage_limit_reached","resets_at":1}}`, wantDelay: 5 * time.Second},
		{name: "exhausted 5h snapshot", body: `{}`, used5h: 100, usedWeekly: 20, wantAt: fiveHourReset},
		{name: "exhausted weekly snapshot", body: `{}`, used5h: 20, usedWeekly: "100", wantAt: weeklyReset},
		{name: "both exhausted wait for both resets", body: `{}`, used5h: 100, usedWeekly: 100, wantAt: weeklyReset},
		{name: "expired exhaustion ignored", body: `{}`, used5h: 100, reset5h: now.Add(-time.Minute), wantDelay: 5 * time.Second},
		{name: "explicit 5h quota error without refreshed usage", body: `{"error":{"code":"1308","message":"Usage limit reached"}}`, used5h: 20, wantAt: fiveHourReset},
		{name: "explicit 5h limit preserves exhausted weekly window", body: `{"error":{"code":1308}}`, used5h: 100, usedWeekly: 100, wantAt: weeklyReset},
		{name: "explicit quota with expired reset falls back", body: `{"error":{"code":1308}}`, reset5h: now.Add(-time.Minute), wantDelay: 5 * time.Second},
		{name: "explicit weekly quota error without refreshed usage", body: `{"error":{"code":1310,"message":"Weekly usage limit reached"}}`, used5h: 20, usedWeekly: 20, wantAt: weeklyReset},
		{name: "payg ignores plan snapshot", body: `{}`, used5h: 100, payg: true, wantDelay: 5 * time.Second},
		{name: "payg honors retry after", headers: http.Header{"Retry-After": {"60"}}, body: `{}`, used5h: 100, payg: true, wantDelay: time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &rateLimitAccountRepoStub{}
			blocker := &runtimeBlockRecorder{}
			svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
			svc.SetAccountRuntimeBlocker(blocker)
			account := &Account{
				ID: 6348, Platform: PlatformZhipu, Type: AccountTypeAPIKey,
				Credentials: map[string]any{"account_mode": AccountModeCoding},
				Extra: map[string]any{
					"zhipu_5h_reset_at":     fiveHourReset.Format(time.RFC3339),
					"zhipu_weekly_reset_at": weeklyReset.Format(time.RFC3339),
				},
			}
			if tt.payg {
				account.Credentials["account_mode"] = AccountModePayG
			}
			if tt.used5h != nil {
				account.Extra["zhipu_5h_used_percent"] = tt.used5h
			}
			if tt.usedWeekly != nil {
				account.Extra["zhipu_weekly_used_percent"] = tt.usedWeekly
			}
			if !tt.reset5h.IsZero() {
				account.Extra["zhipu_5h_reset_at"] = tt.reset5h.Format(time.RFC3339)
			}

			before := time.Now()
			disabled := svc.HandleUpstreamError(context.Background(), account, http.StatusTooManyRequests, tt.headers, []byte(tt.body))
			after := time.Now()

			require.False(t, disabled)
			require.Zero(t, repo.setErrorCalls)
			require.Zero(t, repo.tempCalls)
			require.Equal(t, 1, repo.rateLimitedCalls)
			require.Equal(t, account.ID, repo.lastRateLimitedID)
			if !tt.wantAt.IsZero() {
				require.True(t, tt.wantAt.Equal(repo.lastRateLimitedAt), "got reset %s, want %s", repo.lastRateLimitedAt, tt.wantAt)
			} else {
				// Body reset timestamps have whole-second precision.
				require.False(t, repo.lastRateLimitedAt.Before(before.Add(tt.wantDelay).Truncate(time.Second)))
				require.False(t, repo.lastRateLimitedAt.After(after.Add(tt.wantDelay)))
			}
			require.Equal(t, []*Account{account}, blocker.accounts)
			require.Equal(t, []time.Time{repo.lastRateLimitedAt}, blocker.until)
		})
	}
}

func TestHandle429_ZhipuInsufficientBalanceRemainsRecoverable(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	account := &Account{
		ID: 6348, Platform: PlatformZhipu, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"account_mode": AccountModePayG},
	}
	svc.handle429(context.Background(), account, http.Header{"Retry-After": {"60"}}, []byte(`{"error":{"message":"余额不足"}}`))

	require.Zero(t, repo.setErrorCalls)
	require.Zero(t, repo.rateLimitedCalls)
	require.Equal(t, 1, repo.tempCalls)
	require.Contains(t, repo.lastTempReason, cnBalanceLowReasonPrefix)
	require.Equal(t, true, repo.lastExtraUpdates["zhipu_balance_low"])
}
