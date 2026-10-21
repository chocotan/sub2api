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

const kimiEngineOverloaded429Body = `{"error":{"message":"The engine is currently overloaded, please try again later","type":"engine_overloaded_error"}}`

func TestHandle429_KimiReset(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	fiveHourReset, weeklyReset, shortReset := now.Add(4*time.Hour), now.Add(5*24*time.Hour), now.Add(time.Minute)
	for _, tt := range []struct {
		name       string
		body       string
		headers    http.Header
		used5h     any
		usedWeekly any
		reset5h    time.Time
		payg       bool
		wantAt     time.Time
		wantDelay  time.Duration
	}{
		{name: "production engine overload with unused quota", body: kimiEngineOverloaded429Body, used5h: 10, usedWeekly: 12, wantDelay: 5 * time.Second},
		{name: "engine overload ignores stale exhausted snapshot", body: kimiEngineOverloaded429Body, used5h: 100, usedWeekly: 100, wantDelay: 5 * time.Second},
		{name: "rate limit error is transient", body: `{"error":{"type":"rate_limit_error","message":"Too many requests"}}`, used5h: 100, wantDelay: 5 * time.Second},
		{name: "rate limit exceeded is transient", body: `{"error":{"type":"rate_limit_exceeded"}}`, used5h: 100, wantDelay: 5 * time.Second},
		{name: "concurrency access terminated is transient", body: fmt.Sprintf(`{"error":{"type":"access_terminated_error","message":%q}}`, kimiConcurrentRequestLimitMessage), used5h: 100, wantDelay: 5 * time.Second},
		{name: "unknown error with low usage is transient", body: `{"error":{"message":"Too many requests"}}`, used5h: 10, usedWeekly: 12, wantDelay: 5 * time.Second},
		{name: "timestamps alone do not prove exhaustion", body: `{}`, wantDelay: 5 * time.Second},
		{name: "retry after seconds", body: kimiEngineOverloaded429Body, headers: http.Header{"Retry-After": {"60"}}, used5h: 100, wantDelay: time.Minute},
		{name: "retry after date", body: kimiEngineOverloaded429Body, headers: http.Header{"Retry-After": {shortReset.Format(http.TimeFormat)}}, wantAt: shortReset},
		{name: "body relative reset", body: `{"error":{"type":"rate_limit_exceeded","resets_in_seconds":60}}`, used5h: 100, wantDelay: time.Minute},
		{name: "body absolute reset", body: fmt.Sprintf(`{"error":{"type":"usage_limit_reached","resets_at":%d}}`, shortReset.Unix()), wantAt: shortReset},
		{name: "header wins over body", body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":7200}}`, headers: http.Header{"Retry-After": {"60"}}, wantDelay: time.Minute},
		{name: "invalid header allows body reset", body: `{"error":{"type":"rate_limit_exceeded","resets_in_seconds":"60"}}`, headers: http.Header{"Retry-After": {"invalid"}}, wantDelay: time.Minute},
		{name: "zero retry after ignored", body: kimiEngineOverloaded429Body, headers: http.Header{"Retry-After": {"0"}}, wantDelay: 5 * time.Second},
		{name: "past retry after ignored", body: kimiEngineOverloaded429Body, headers: http.Header{"Retry-After": {now.Add(-time.Minute).Format(http.TimeFormat)}}, wantDelay: 5 * time.Second},
		{name: "five hour quota exhausted", body: `{}`, used5h: 100, usedWeekly: 12, wantAt: fiveHourReset},
		{name: "weekly quota exhausted", body: `{}`, used5h: 10, usedWeekly: "100", wantAt: weeklyReset},
		{name: "both windows exhausted", body: `{}`, used5h: 100, usedWeekly: 100, wantAt: weeklyReset},
		{name: "expired exhaustion ignored", body: `{}`, used5h: 100, reset5h: now.Add(-time.Minute), wantDelay: 5 * time.Second},
		{name: "explicit quota signal before usage refresh", body: kimiWeeklyQuotaExhaustedBody, used5h: 10, usedWeekly: 12, wantAt: fiveHourReset},
		{name: "explicit quota signal preserves exhausted weekly", body: kimiWeeklyQuotaExhaustedBody, used5h: 100, usedWeekly: 100, wantAt: weeklyReset},
		{name: "payg ignores plan snapshot", body: `{}`, payg: true, used5h: 100, wantDelay: 5 * time.Second},
		{name: "payg honors upstream retry", body: kimiEngineOverloaded429Body, headers: http.Header{"Retry-After": {"60"}}, payg: true, wantDelay: time.Minute},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := &rateLimitAccountRepoStub{}
			blocker := &runtimeBlockRecorder{}
			svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
			svc.SetAccountRuntimeBlocker(blocker)
			account := &Account{ID: 21, Platform: PlatformKimi, Type: AccountTypeAPIKey,
				Credentials: map[string]any{"account_mode": AccountModeCoding},
				Extra:       map[string]any{"kimi_5h_reset_at": fiveHourReset.Format(time.RFC3339), "kimi_weekly_reset_at": weeklyReset.Format(time.RFC3339)},
			}
			if tt.used5h != nil {
				account.Extra["kimi_5h_used_percent"] = tt.used5h
			}
			if tt.usedWeekly != nil {
				account.Extra["kimi_weekly_used_percent"] = tt.usedWeekly
			}
			if tt.payg {
				account.Credentials["account_mode"] = AccountModePayG
			}
			if !tt.reset5h.IsZero() {
				account.Extra["kimi_5h_reset_at"] = tt.reset5h.Format(time.RFC3339)
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
				require.True(t, tt.wantAt.Equal(repo.lastRateLimitedAt), "got %s, want %s", repo.lastRateLimitedAt, tt.wantAt)
			} else {
				require.False(t, repo.lastRateLimitedAt.Before(before.Add(tt.wantDelay).Truncate(time.Second)))
				require.False(t, repo.lastRateLimitedAt.After(after.Add(tt.wantDelay)))
			}
			require.Equal(t, []*Account{account}, blocker.accounts)
			require.Equal(t, []time.Time{repo.lastRateLimitedAt}, blocker.until)
		})
	}
}

func TestHandle429_KimiInsufficientBalanceRemainsRecoverable(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	account := &Account{ID: 21, Platform: PlatformKimi, Type: AccountTypeAPIKey, Credentials: map[string]any{"account_mode": AccountModePayG}}
	svc.handle429(context.Background(), account, http.Header{"Retry-After": {"60"}}, []byte(`{"error":{"message":"insufficient balance"}}`))
	require.Zero(t, repo.setErrorCalls)
	require.Zero(t, repo.rateLimitedCalls)
	require.Equal(t, 1, repo.tempCalls)
	require.Contains(t, repo.lastTempReason, cnBalanceLowReasonPrefix)
	require.Equal(t, true, repo.lastExtraUpdates["kimi_balance_low"])
}
