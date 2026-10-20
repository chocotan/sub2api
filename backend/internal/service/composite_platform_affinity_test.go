package service

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type compositeAffinityCache struct {
	GatewayCache
	bindings map[string]int64
	err      error
}

func (c *compositeAffinityCache) GetSessionAccountID(_ context.Context, groupID int64, hash string) (int64, error) {
	if c.err != nil {
		return 0, c.err
	}
	return c.bindings[fmt.Sprintf("%d:%s", groupID, hash)], nil
}

func (c *compositeAffinityCache) SetSessionAccountID(_ context.Context, groupID int64, hash string, accountID int64, _ time.Duration) error {
	c.bindings[fmt.Sprintf("%d:%s", groupID, hash)] = accountID
	return nil
}

func compositeAffinityRequest(t *testing.T, path string, body []byte, header string) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", path, nil)
	if header != "" {
		c.Request.Header.Set("X-Session-Affinity", header)
	}
	c.Request = c.Request.WithContext(WithCompositeSessionAffinity(c.Request.Context(), c, body))
	return c
}

func TestCompositeAPIKeyPlatformPreservesSessionAffinity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	model := "deepseek-v4.1-flash"
	firstBody := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hello"}]}`)
	nextBody := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"continue"}]}`)
	future := time.Now().Add(time.Hour)
	for _, tt := range []struct {
		name          string
		header        string
		bindGroup     int64
		bindID        int64
		legacy        bool
		disableLegacy bool
		cacheError    bool
		change        func([]Account)
		want          string
	}{
		{name: "content derived session", bindGroup: 21, bindID: 37, want: PlatformCline},
		{name: "explicit session header", header: "session-123", bindGroup: 21, bindID: 37, want: PlatformCline},
		{name: "legacy binding", bindGroup: 21, bindID: 37, legacy: true, want: PlatformCline},
		{name: "legacy fallback disabled", bindGroup: 21, bindID: 37, legacy: true, disableLegacy: true, want: PlatformOpenCodeGo},
		{name: "other group binding ignored", bindGroup: 99, bindID: 37, want: PlatformOpenCodeGo},
		{name: "binding outside claimant pool ignored", bindGroup: 21, bindID: 999, want: PlatformOpenCodeGo},
		{name: "disabled binding ignored", bindGroup: 21, bindID: 37, change: func(a []Account) { a[1].Schedulable = false }, want: PlatformOpenCodeGo},
		{name: "rate limited binding ignored", bindGroup: 21, bindID: 37, change: func(a []Account) { a[1].RateLimitResetAt = &future }, want: PlatformOpenCodeGo},
		{name: "model limited binding ignored", bindGroup: 21, bindID: 37, change: func(a []Account) {
			setAccountModelRateLimitSnapshot(&a[1], "cline-pass/"+model, future, "429", time.Now())
		}, want: PlatformOpenCodeGo},
		{name: "quota exhausted binding ignored", bindGroup: 21, bindID: 37, change: func(a []Account) {
			a[1].Extra = map[string]any{"quota_limit": 1.0, "quota_used": 1.0}
		}, want: PlatformOpenCodeGo},
		{name: "cache failure falls back to load selection", bindGroup: 21, bindID: 37, cacheError: true, want: PlatformOpenCodeGo},
	} {
		t.Run(tt.name, func(t *testing.T) {
			accounts := []Account{
				{ID: 22, Platform: PlatformOpenCodeGo, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 1,
					Credentials: map[string]any{"model_mapping": map[string]any{model: model}}},
				{ID: 37, Platform: PlatformCline, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 2,
					Credentials: map[string]any{"model_mapping": map[string]any{model: "cline-pass/" + model}}},
			}
			cache := &compositeAffinityCache{bindings: map[string]int64{}}
			gateway := &OpenAIGatewayService{cache: cache}
			first := compositeAffinityRequest(t, "/v1/chat/completions", firstBody, tt.header)
			hash := gateway.GenerateSessionHash(first, firstBody)
			require.NoError(t, gateway.BindStickySession(first.Request.Context(), &tt.bindGroup, hash, tt.bindID))
			if tt.legacy {
				delete(cache.bindings, fmt.Sprintf("%d:openai:%s", tt.bindGroup, hash))
			}
			if tt.cacheError {
				cache.err = errors.New("redis unavailable")
			}
			if tt.change != nil {
				tt.change(accounts)
			}
			svc := &GatewayService{
				accountRepo: &compositeOwnershipAccountRepo{accounts: accounts}, cache: cache,
				// A busy but healthy sticky account must reach the original platform's wait policy.
				concurrencyService: &ConcurrencyService{cache: &compositePlatformLoadCache{loads: map[int64]*AccountLoadInfo{
					22: {AccountID: 22, LoadRate: 0}, 37: {AccountID: 37, LoadRate: 100},
				}}},
			}
			if tt.disableLegacy {
				svc.cfg = &config.Config{}
			}
			next := compositeAffinityRequest(t, "/v1/chat/completions", nextBody, tt.header)
			require.Equal(t, hash, gateway.GenerateSessionHash(next, nextBody))
			resolver := NewCompositeRouteResolver(nil)
			resolver.SetModelOwnershipResolver(svc.resolveCompositeModelOwnership)
			decision, err := resolver.Resolve(next.Request.Context(), 21, model, CompositeRouteEndpointChatCompletions)
			require.NoError(t, err)
			require.True(t, decision.Matched)
			require.Equal(t, tt.want, decision.TargetPlatform)
		})
	}
}

func TestCompositeMessagesClaudeCodeSessionAffinity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, platform := range []string{PlatformCline, PlatformGrok} {
		t.Run(platform, func(t *testing.T) {
			model := "deepseek-v4.1-flash"
			firstBody := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hello"}]}`)
			nextBody := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"compacted conversation"}]}`)
			cache := &compositeAffinityCache{bindings: map[string]int64{}}
			gateway := &OpenAIGatewayService{cache: cache, cfg: &config.Config{}}
			groupID := int64(21)
			first, _ := gin.CreateTestContext(httptest.NewRecorder())
			first.Request = httptest.NewRequest("POST", "/v1/messages", nil)
			first.Request.Header.Set("X-Claude-Code-Session-Id", "claude-session-123")
			first.Request = first.Request.WithContext(WithResolvedTargetPlatform(first.Request.Context(), platform))
			hash := gateway.GenerateSessionHash(first, firstBody)
			hash, _ = ResolveOpenAIMessagesMetadataSession(first, hash, gateway.ExtractSessionID(first, firstBody), model, firstBody)
			require.Equal(t, DeriveSessionHashFromSeed("claude-session-123"), hash)
			require.NoError(t, gateway.BindStickySession(first.Request.Context(), &groupID, hash, 37))
			// No legacy binding is written: the provider lookup must use the final Messages hash.
			require.Len(t, cache.bindings, 1)
			next, _ := gin.CreateTestContext(httptest.NewRecorder())
			next.Request = httptest.NewRequest("POST", "/v1/messages", nil)
			next.Request.Header.Set("X-Claude-Code-Session-Id", "claude-session-123")
			next.Request = next.Request.WithContext(WithCompositeSessionAffinity(next.Request.Context(), next, nextBody))
			svc := &GatewayService{cache: cache, cfg: &config.Config{}, accountRepo: &compositeOwnershipAccountRepo{accounts: []Account{
				{ID: 22, Platform: PlatformOpenCodeGo, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 1,
					Credentials: map[string]any{"model_mapping": map[string]any{model: model}}},
				{ID: 37, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 2,
					Credentials: map[string]any{"model_mapping": map[string]any{model: model}}},
			}}}
			ownership, err := svc.resolveCompositeModelOwnership(next.Request.Context(), groupID, model)
			require.NoError(t, err)
			require.Equal(t, platform, ownership.TargetPlatform)
		})
	}
}

func TestCompositeExplicitRouteOverridesSessionAffinity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	model := "deepseek-v4.1-flash"
	body := []byte(`{"model":"deepseek-v4.1-flash","prompt_cache_key":"session-123"}`)
	c := compositeAffinityRequest(t, "/v1/chat/completions", body, "")
	cache := &compositeAffinityCache{bindings: map[string]int64{}}
	gateway := &OpenAIGatewayService{cache: cache}
	groupID := int64(21)
	hash := gateway.GenerateSessionHash(c, body)
	require.NoError(t, gateway.BindStickySession(c.Request.Context(), &groupID, hash, 37))
	svc := &GatewayService{cache: cache}
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{{
		ID: 1, GroupID: groupID, PublicModel: model, MatchType: CompositeRouteMatchExact,
		TargetPlatform: PlatformOpenCodeGo, UpstreamModel: model, Endpoint: CompositeRouteEndpointAny, Enabled: true,
	}}})
	resolver.SetModelOwnershipResolver(svc.resolveCompositeModelOwnership)
	decision, err := resolver.Resolve(c.Request.Context(), groupID, model, CompositeRouteEndpointChatCompletions)
	require.NoError(t, err)
	require.Equal(t, CompositeRouteSourceExplicit, decision.Source)
	require.Equal(t, PlatformOpenCodeGo, decision.TargetPlatform)
}

func TestCompositeSessionAffinityMatchesSchedulerHash(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, platform := range []string{PlatformOpenCodeGo, PlatformCline, PlatformGrok} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
			t.Run(platform+path, func(t *testing.T) {
				body := []byte(`{"model":"shared-model","prompt_cache_key":"cache-session","messages":[{"role":"user","content":"hello"}]}`)
				c := compositeAffinityRequest(t, path, body, "")
				cache := &compositeAffinityCache{bindings: map[string]int64{}}
				gateway := &OpenAIGatewayService{cache: cache}
				c.Request = c.Request.WithContext(WithResolvedTargetPlatform(c.Request.Context(), platform))
				hash := gateway.GenerateSessionHash(c, body)
				groupID := int64(21)
				require.NoError(t, gateway.BindStickySession(c.Request.Context(), &groupID, hash, 37))
				affinity := c.Request.Context().Value(compositeSessionAffinityKey{}).(compositeSessionAffinity)
				svc := &GatewayService{cache: cache}
				require.Equal(t, int64(37), svc.compositeStickyAccountID(c.Request.Context(), groupID, affinity, platform))
			})
		}
	}
}
