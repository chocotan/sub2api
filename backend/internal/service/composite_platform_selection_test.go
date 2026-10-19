package service

import (
	"context"
	"errors"
	"testing"
	"time"

	gocache "github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"
)

type compositePlatformLoadCache struct {
	ConcurrencyCache
	loads map[int64]*AccountLoadInfo
	err   error
}

func (c *compositePlatformLoadCache) GetAccountsLoadBatch(context.Context, []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error) {
	return c.loads, c.err
}

func TestCompositeMappedAPIKeysChooseServingPlatform(t *testing.T) {
	model := "deepseek-v4.1-flash"
	now := time.Now()
	older, newer, future := now.Add(-time.Hour), now.Add(-time.Minute), now.Add(time.Hour)
	for _, tt := range []struct {
		name    string
		change  func([]Account)
		loads   map[int64]*AccountLoadInfo
		loadErr error
		want    string
	}{
		{name: "priority", want: PlatformOpenCodeGo},
		{name: "LRU", change: func(a []Account) {
			a[1].Priority = a[0].Priority
			a[0].LastUsedAt, a[1].LastUsedAt = &newer, &older
		}, want: PlatformCline},
		{name: "load", change: func(a []Account) { a[1].Priority = a[0].Priority }, loads: map[int64]*AccountLoadInfo{
			22: {AccountID: 22, LoadRate: 80}, 37: {AccountID: 37, LoadRate: 20},
		}, want: PlatformCline},
		{name: "full higher priority provider", loads: map[int64]*AccountLoadInfo{
			22: {AccountID: 22, LoadRate: 100}, 37: {AccountID: 37, LoadRate: 20},
		}, want: PlatformCline},
		{name: "load lookup fails uses priority", loadErr: errors.New("redis unavailable"), want: PlatformOpenCodeGo},
		{name: "rate limited", change: func(a []Account) { a[0].RateLimitResetAt = &future }, want: PlatformCline},
		{name: "overloaded", change: func(a []Account) { a[0].OverloadUntil = &future }, want: PlatformCline},
		{name: "temporary cooldown", change: func(a []Account) { a[0].TempUnschedulableUntil = &future }, want: PlatformCline},
		{name: "expired", change: func(a []Account) {
			a[0].AutoPauseOnExpired, a[0].ExpiresAt = true, &older
		}, want: PlatformCline},
		{name: "quota exhausted", change: func(a []Account) {
			a[0].Extra = map[string]any{"quota_limit": 1.0, "quota_used": 1.0}
		}, want: PlatformCline},
		{name: "model cooldown", change: func(a []Account) {
			setAccountModelRateLimitSnapshot(&a[0], model, future, "upstream rate limit", now)
		}, want: PlatformCline},
		{name: "disabled", change: func(a []Account) { a[0].Schedulable = false }, want: PlatformCline},
		{name: "inactive", change: func(a []Account) { a[0].Status = "inactive" }, want: PlatformCline},
		{name: "all limited stays with mapped provider", change: func(a []Account) {
			a[0].RateLimitResetAt, a[1].RateLimitResetAt = &future, &future
		}, want: PlatformOpenCodeGo},
	} {
		t.Run(tt.name, func(t *testing.T) {
			accounts := []Account{
				{ID: 22, Platform: PlatformOpenCodeGo, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 1,
					Credentials: map[string]any{"model_mapping": map[string]any{model: model}}},
				{ID: 37, Platform: PlatformCline, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 2,
					Credentials: map[string]any{"model_mapping": map[string]any{model: "cline-pass/" + model}}},
				{ID: 45, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: false, Priority: 0,
					Credentials: map[string]any{"model_mapping": map[string]any{model: "deepseek-flash"}}},
			}
			if tt.change != nil {
				tt.change(accounts)
			}
			svc := &GatewayService{accountRepo: &compositeOwnershipAccountRepo{accounts: accounts}}
			if tt.loads != nil || tt.loadErr != nil {
				svc.concurrencyService = &ConcurrencyService{cache: &compositePlatformLoadCache{loads: tt.loads, err: tt.loadErr}}
			}
			resolver := NewCompositeRouteResolver(nil)
			resolver.SetModelOwnershipResolver(svc.resolveCompositeModelOwnership)
			decision, err := resolver.Resolve(context.Background(), 21, model, CompositeRouteEndpointChatCompletions)
			require.NoError(t, err)
			require.True(t, decision.Matched)
			require.Equal(t, CompositeRouteSourceAccount, decision.Source)
			require.Equal(t, tt.want, decision.TargetPlatform)
			require.Equal(t, model, decision.UpstreamModel)
		})
	}
}

func TestCompositeMultiPlatformChoiceIsNotCached(t *testing.T) {
	model := "deepseek-v4.1-flash"
	repo := &compositeOwnershipAccountRepo{accounts: []Account{
		{ID: 22, Platform: PlatformOpenCodeGo, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 1,
			Credentials: map[string]any{"model_mapping": map[string]any{model: model}}},
		{ID: 37, Platform: PlatformCline, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 2,
			Credentials: map[string]any{"model_mapping": map[string]any{model: "cline-pass/" + model}}},
	}}
	svc := &GatewayService{accountRepo: repo, modelsListCache: gocache.New(time.Minute, time.Minute), modelsListCacheTTL: time.Minute}
	first, err := svc.resolveCompositeModelOwnership(context.Background(), 21, model)
	require.NoError(t, err)
	require.Equal(t, PlatformOpenCodeGo, first.TargetPlatform)

	future := time.Now().Add(time.Minute)
	repo.accounts[0].RateLimitResetAt = &future
	second, err := svc.resolveCompositeModelOwnership(context.Background(), 21, model)
	require.NoError(t, err)
	require.Equal(t, PlatformCline, second.TargetPlatform)

	repo.accounts[0].RateLimitResetAt = nil
	third, err := svc.resolveCompositeModelOwnership(context.Background(), 21, model)
	require.NoError(t, err)
	require.Equal(t, PlatformOpenCodeGo, third.TargetPlatform)
}
