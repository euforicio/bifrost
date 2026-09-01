package providercredentials

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestOpenAIUsageNormalizesWindowsCreditsAndResetDetails(t *testing.T) {
	fetchedAt := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer access-token", r.Header.Get("Authorization"))
		require.Equal(t, "account-123", r.Header.Get("ChatGPT-Account-ID"))
		switch r.URL.Path {
		case openAIUsagePath:
			_, _ = io.WriteString(w, `{
				"rate_limit":{"primary_window":{"used_percent":25.5,"limit_window_seconds":18000,"reset_at":1787940000},"secondary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_at":1788544800}},
				"additional_rate_limits":[{"metered_feature":"review","limit_name":"Code review","rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":3600,"reset_at":1787930000}}}],
				"credits":{"has_credits":true,"unlimited":false,"balance":"12.50"},
				"rate_limit_reset_credits":{"available_count":1}
			}`)
		case openAIResetCreditsPath:
			_, _ = io.WriteString(w, `{"available_count":1,"credits":[{"id":"reset-1","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-08-01T00:00:00Z","expires_at":"2026-09-01T00:00:00Z","title":"Full reset","description":"Ready"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	_, db := newTestManager(t, server)
	manager := NewManager(databaseStore{db: db}, WithHTTPClient(server.Client()), WithUsageEndpoints(server.URL, server.URL), WithNow(func() time.Time { return fetchedAt }))
	expiresAt := fetchedAt.Add(time.Hour)
	require.NoError(t, db.Create(&tables.TableProviderCredential{
		CredentialID: "openai-key", Provider: string(ProviderOpenAICodex), ProviderKeyID: "openai-key", AuthMode: "device_code",
		AccessToken: "access-token", RefreshToken: "refresh-token", AccountID: "account-123", ExpiresAt: &expiresAt, Status: StatusConnected, Version: 1,
	}).Error)

	usage := manager.Usage(context.Background(), ProviderOpenAICodex, "openai-key")
	require.Equal(t, UsageAvailable, usage.Availability)
	require.Equal(t, fetchedAt, *usage.FetchedAt)
	require.Len(t, usage.Quotas, 3)
	require.Equal(t, "codex:primary", usage.Quotas[0].ID)
	require.Equal(t, 25.5, *usage.Quotas[0].UsedPercent)
	require.Equal(t, int64(300), *usage.Quotas[0].WindowDurationMinutes)
	require.NotNil(t, usage.Quotas[0].StartsAt)
	require.Equal(t, "review:primary", usage.Quotas[2].ID)
	require.Equal(t, 12.5, *usage.Credits.Balance)
	require.Equal(t, int64(1), usage.ResetCredits.AvailableCount)
	require.Len(t, usage.ResetCredits.Credits, 1)
	require.Equal(t, "reset-1", usage.ResetCredits.Credits[0].ID)
	require.Equal(t, "Full reset", usage.ResetCredits.Credits[0].Title)
}

func TestOpenAIUsageRetriesOneUnauthorizedWithForcedRefresh(t *testing.T) {
	var usageRequests atomic.Int64
	var refreshRequests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case openAIUsagePath:
			usageRequests.Add(1)
			if r.Header.Get("Authorization") == "Bearer stale-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			require.Equal(t, "Bearer fresh-token", r.Header.Get("Authorization"))
			_, _ = io.WriteString(w, `{"rate_limit":{}}`)
		case "/oauth/token":
			refreshRequests.Add(1)
			_, _ = io.WriteString(w, `{"access_token":"fresh-token","refresh_token":"fresh-refresh","expires_in":3600}`)
		case openAIResetCreditsPath:
			w.WriteHeader(http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	_, db := newTestManager(t, server)
	manager := NewManager(databaseStore{db: db}, WithHTTPClient(server.Client()), WithEndpoints(server.URL, "", ""), WithUsageEndpoints(server.URL, server.URL))
	expiresAt := time.Now().Add(time.Hour)
	require.NoError(t, db.Create(&tables.TableProviderCredential{
		CredentialID: "openai-key", Provider: string(ProviderOpenAICodex), ProviderKeyID: "openai-key", AuthMode: "device_code",
		AccessToken: "stale-token", RefreshToken: "refresh-token", ExpiresAt: &expiresAt, Status: StatusConnected, Version: 1,
	}).Error)

	usage := manager.Usage(context.Background(), ProviderOpenAICodex, "openai-key")
	require.Equal(t, UsageAvailable, usage.Availability)
	require.Equal(t, int64(2), usageRequests.Load())
	require.Equal(t, int64(1), refreshRequests.Load())
}

func TestUsageFailureDoesNotChangeInferenceCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	_, db := newTestManager(t, server)
	manager := NewManager(databaseStore{db: db}, WithHTTPClient(server.Client()), WithUsageEndpoints(server.URL, server.URL))
	expiresAt := time.Now().Add(time.Hour)
	require.NoError(t, db.Create(&tables.TableProviderCredential{
		CredentialID: "openai-key", Provider: string(ProviderOpenAICodex), ProviderKeyID: "openai-key", AuthMode: "device_code",
		AccessToken: "inference-token", RefreshToken: "refresh-token", ExpiresAt: &expiresAt, Status: StatusConnected, Version: 1,
	}).Error)

	usage := manager.Usage(context.Background(), ProviderOpenAICodex, "openai-key")
	require.Equal(t, UsageUnavailable, usage.Availability)
	require.Equal(t, usageUnavailableMessage, usage.Message)
	resolved, err := manager.ResolveProviderCredential(context.Background(), ProviderOpenAICodex, "openai-key", false)
	require.NoError(t, err)
	require.Equal(t, "inference-token", resolved.AccessToken)
}

func TestUsageUnauthorizedWithRefreshFailureDoesNotChangeInferenceCredential(t *testing.T) {
	var refreshRequests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case openAIUsagePath:
			w.WriteHeader(http.StatusUnauthorized)
		case "/oauth/token":
			refreshRequests.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	_, db := newTestManager(t, server)
	manager := NewManager(databaseStore{db: db}, WithHTTPClient(server.Client()), WithEndpoints(server.URL, "", ""), WithUsageEndpoints(server.URL, server.URL))
	expiresAt := time.Now().Add(time.Hour)
	record := tables.TableProviderCredential{
		CredentialID: "openai-key", Provider: string(ProviderOpenAICodex), ProviderKeyID: "openai-key", AuthMode: "device_code",
		AccessToken: "inference-token", RefreshToken: "refresh-token", AccountID: "account-123", ExpiresAt: &expiresAt,
		Status: StatusConnected, Version: 7,
	}
	require.NoError(t, db.Create(&record).Error)

	usage := manager.Usage(context.Background(), ProviderOpenAICodex, "openai-key")
	require.Equal(t, UsageUnavailable, usage.Availability)
	require.Equal(t, int64(1), refreshRequests.Load())

	var after tables.TableProviderCredential
	require.NoError(t, db.Where("provider = ? AND credential_id = ?", string(ProviderOpenAICodex), "openai-key").First(&after).Error)
	require.Equal(t, StatusConnected, after.Status)
	require.Equal(t, uint64(7), after.Version)
	require.Equal(t, "inference-token", after.AccessToken)
	require.Equal(t, "refresh-token", after.RefreshToken)
	require.Equal(t, "account-123", after.AccountID)
	require.Equal(t, expiresAt.Unix(), after.ExpiresAt.Unix())
	require.Empty(t, after.RefreshLeaseOwner)
	require.Nil(t, after.RefreshLeaseExpiresAt)
}

func TestOpenAIUsagePreservesResetCountWhenDetailsAreUnavailable(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case openAIUsagePath:
			_, _ = io.WriteString(w, `{"rate_limit":{},"rate_limit_reset_credits":{"available_count":2}}`)
		case openAIResetCreditsPath:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	_, db := newTestManager(t, server)
	manager := NewManager(databaseStore{db: db}, WithHTTPClient(server.Client()), WithUsageEndpoints(server.URL, server.URL))
	expiresAt := time.Now().Add(time.Hour)
	require.NoError(t, db.Create(&tables.TableProviderCredential{
		CredentialID: "openai-key", Provider: string(ProviderOpenAICodex), ProviderKeyID: "openai-key", AuthMode: "device_code",
		AccessToken: "access-token", RefreshToken: "refresh-token", ExpiresAt: &expiresAt, Status: StatusConnected, Version: 1,
	}).Error)

	usage := manager.Usage(context.Background(), ProviderOpenAICodex, "openai-key")
	require.Equal(t, UsageAvailable, usage.Availability)
	require.NotNil(t, usage.ResetCredits)
	require.Equal(t, int64(2), usage.ResetCredits.AvailableCount)
	require.NotNil(t, usage.ResetCredits.Credits)
	require.Empty(t, usage.ResetCredits.Credits)
}

func TestXAIUsageIsUnsupportedWithoutNetworkProbe(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	manager := NewManager(nil, WithHTTPClient(server.Client()), WithUsageEndpoints(server.URL, server.URL))

	usage := manager.Usage(context.Background(), ProviderXAI, "xai-key")
	require.Equal(t, UsageUnsupported, usage.Availability)
	require.Equal(t, xAIUsageUnsupportedMessage, usage.Message)
	require.Empty(t, usage.Quotas)
	require.Zero(t, requests.Load())
}

func TestCursorUsageSendsProtocolHeadersDecodesFieldsAndRetriesOnce(t *testing.T) {
	fetchedAt := time.Now().UTC().Truncate(time.Second)
	var usageRequests atomic.Int64
	var refreshRequests atomic.Int64
	response := cursorUsageFixture(t)
	planResponse := cursorPlanInfoFixture()
	sandResponse := cursorSandUsageFixture()
	hardLimitResponse := cursorHardLimitFixture()
	_, parseErr := parseCursorUsage(response, "cursor-key", fetchedAt)
	require.NoError(t, parseErr)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cursorCurrentPeriodUsagePath:
			usageRequests.Add(1)
			require.Equal(t, http.MethodPost, r.Method)
			require.Equal(t, "application/proto", r.Header.Get("Content-Type"))
			require.Equal(t, "1", r.Header.Get("Connect-Protocol-Version"))
			require.Equal(t, "cli", r.Header.Get("X-Cursor-Client-Type"))
			require.NotEmpty(t, r.Header.Get("X-Cursor-Client-Version"))
			require.Equal(t, "true", r.Header.Get("X-Ghost-Mode"))
			require.NotEmpty(t, r.Header.Get("X-Request-ID"))
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.Empty(t, body)
			if r.Header.Get("Authorization") == "Bearer stale-token" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			require.Equal(t, "Bearer fresh-token", r.Header.Get("Authorization"))
			_, _ = w.Write(response)
		case cursorPlanInfoPath:
			require.Equal(t, "Bearer fresh-token", r.Header.Get("Authorization"))
			_, _ = w.Write(planResponse)
		case cursorSandUsageStatusPath:
			require.Equal(t, "Bearer fresh-token", r.Header.Get("Authorization"))
			_, _ = w.Write(sandResponse)
		case cursorHardLimitPath:
			require.Equal(t, "Bearer fresh-token", r.Header.Get("Authorization"))
			_, _ = w.Write(hardLimitResponse)
		case cursorRefreshPath:
			refreshRequests.Add(1)
			require.Equal(t, "Bearer cursor-refresh", r.Header.Get("Authorization"))
			_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "fresh-token", "refreshToken": "fresh-refresh"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	_, db := newTestManager(t, server)
	manager := NewManager(databaseStore{db: db}, WithHTTPClient(server.Client()), WithCursorEndpoints(server.URL, server.URL), WithUsageEndpoints("", server.URL), WithNow(func() time.Time { return fetchedAt }))
	expiresAt := fetchedAt.Add(time.Hour)
	require.NoError(t, db.Create(&tables.TableProviderCredential{
		CredentialID: "cursor-key", Provider: string(ProviderCursor), ProviderKeyID: "cursor-key", AuthMode: "pkce_browser",
		AccessToken: "stale-token", RefreshToken: "cursor-refresh", ExpiresAt: &expiresAt, Status: StatusConnected, Version: 1,
	}).Error)

	usage := manager.Usage(context.Background(), ProviderCursor, "cursor-key")
	require.Equal(t, UsageAvailable, usage.Availability)
	require.Equal(t, int64(2), usageRequests.Load())
	require.Equal(t, int64(1), refreshRequests.Load())
	require.Len(t, usage.Quotas, 6)
	require.Equal(t, "cents", usage.Quotas[0].Unit)
	require.Equal(t, float64(1000), *usage.Quotas[0].Used)
	require.Equal(t, float64(5000), *usage.Quotas[0].Limit)
	require.Equal(t, 25.0, *usage.Quotas[0].UsedPercent)
	require.Equal(t, "cursor-models", usage.Quotas[1].ID)
	require.Equal(t, "Cursor Models", usage.Quotas[1].Name)
	require.Equal(t, "Includes Cursor Grok and Composer", usage.Quotas[1].Description)
	require.Equal(t, "other-models", usage.Quotas[2].ID)
	require.Equal(t, "Other Models", usage.Quotas[2].Name)
	require.Equal(t, "spend-limit:overall", usage.Quotas[4].ID)
	require.Equal(t, "grok-bot", usage.Quotas[5].ID)
	require.Equal(t, 1.0, *usage.Quotas[5].UsedPercent)
	require.Equal(t, "Super Grok", usage.Quotas[5].Description)
	require.NotNil(t, usage.Quotas[0].StartsAt)
	require.NotNil(t, usage.Quotas[0].ResetsAt)
	require.NotNil(t, usage.Plan)
	require.Equal(t, "Ultra", usage.Plan.Name)
	require.Equal(t, "$200/mo", usage.Plan.Price)
	require.NotNil(t, usage.Plan.BillingCycleEnd)
	require.NotNil(t, usage.OnDemand)
	require.True(t, usage.OnDemand.Enabled)
	require.Equal(t, float64(5000), *usage.OnDemand.Limit)
	require.NotNil(t, usage.ResetCredits)
	require.Equal(t, int64(2), usage.ResetCredits.AvailableCount)
}

func TestCursorUsageWithoutPlanIsUnavailable(t *testing.T) {
	// Valid response containing only the billing period (fields 1 and 2).
	response, err := hex.DecodeString("0880b0fbd4fb331080f88fd28534")
	require.NoError(t, err)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == cursorCurrentPeriodUsagePath {
			_, _ = w.Write(response)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	_, db := newTestManager(t, server)
	manager := NewManager(databaseStore{db: db}, WithHTTPClient(server.Client()), WithCursorEndpoints(server.URL, server.URL), WithUsageEndpoints("", server.URL))
	expiresAt := time.Now().Add(time.Hour)
	require.NoError(t, db.Create(&tables.TableProviderCredential{
		CredentialID: "cursor-key", Provider: string(ProviderCursor), ProviderKeyID: "cursor-key", AuthMode: "pkce_browser",
		AccessToken: "access-token", RefreshToken: "refresh-token", ExpiresAt: &expiresAt, Status: StatusConnected, Version: 1,
	}).Error)

	usage := manager.Usage(context.Background(), ProviderCursor, "cursor-key")
	require.Equal(t, UsageUnavailable, usage.Availability)
	require.Empty(t, usage.Quotas)
}

func TestCursorUsagePreservesCurrentPeriodWhenAuxiliaryReadsFail(t *testing.T) {
	response := cursorUsageFixture(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == cursorCurrentPeriodUsagePath {
			_, _ = w.Write(response)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	_, db := newTestManager(t, server)
	manager := NewManager(databaseStore{db: db}, WithHTTPClient(server.Client()), WithCursorEndpoints(server.URL, server.URL), WithUsageEndpoints("", server.URL))
	expiresAt := time.Now().Add(time.Hour)
	require.NoError(t, db.Create(&tables.TableProviderCredential{
		CredentialID: "cursor-key", Provider: string(ProviderCursor), ProviderKeyID: "cursor-key", AuthMode: "pkce_browser",
		AccessToken: "access-token", RefreshToken: "refresh-token", ExpiresAt: &expiresAt, Status: StatusConnected, Version: 1,
	}).Error)

	usage := manager.Usage(context.Background(), ProviderCursor, "cursor-key")
	require.Equal(t, UsageAvailable, usage.Availability)
	require.Len(t, usage.Quotas, 5)
	require.Nil(t, usage.Plan)
	require.NotNil(t, usage.OnDemand)
}

func cursorUsageFixture(t *testing.T) []byte {
	t.Helper()
	// Immutable descriptor-backed fixture for GetCurrentPeriodUsageResponse.
	// Field numbers and wire types come from Cursor Agent CLI
	// 2026.08.11-e8db854 (SHA-256
	// 6aceb24b7c7ecddb1993946ebb18a7dd4d025842e6efda955eb0c13255b1e5f0).
	// Field 100 is intentionally unknown to verify forward-compatible parsing.
	response, err := hex.DecodeString("0880b0fbd4fb331080f88fd285341a3908e20910e80718fa0120a61d28882740a00648c20350b81758d00f610000000000003440690000000000803640710000000000003940980607222a08bc0510904e18a01f20f02e28882730bc0538cc21420a696e646976696475616c48987550dc2458bc50a2060769676e6f726564")
	require.NoError(t, err)
	response = appendProtoString(response, 13, "cursor-grok-4.5")
	response = appendProtoString(response, 13, "composer-2.5")
	return response
}

func cursorPlanInfoFixture() []byte {
	plan := appendProtoString(nil, 1, "Ultra")
	plan = appendProtoVarint(plan, 2, 40000)
	plan = appendProtoString(plan, 3, "$200/mo")
	plan = appendProtoVarint(plan, 4, uint64(time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC).UnixMilli()))
	return appendProtoMessage(nil, 1, plan)
}

func cursorSandUsageFixture() []byte {
	start := appendProtoVarint(nil, 1, uint64(time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC).Unix()))
	reset := appendProtoVarint(nil, 1, uint64(time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC).Unix()))
	response := appendProtoMessage(nil, 1, start)
	response = appendProtoMessage(response, 2, reset)
	response = protowire.AppendTag(response, 3, protowire.Fixed64Type)
	response = protowire.AppendFixed64(response, math.Float64bits(1))
	response = appendProtoVarint(response, 5, 2)
	response = appendProtoString(response, 14, "Super Grok")
	response = appendProtoString(response, 15, "Super Grok")
	return response
}

func cursorHardLimitFixture() []byte {
	response := appendProtoVarint(nil, 2, 0)
	response = appendProtoVarint(response, 4, 100)
	response = appendProtoVarint(response, 10, 0)
	return response
}

func appendProtoVarint(dst []byte, number protowire.Number, value uint64) []byte {
	dst = protowire.AppendTag(dst, number, protowire.VarintType)
	return protowire.AppendVarint(dst, value)
}

func appendProtoString(dst []byte, number protowire.Number, value string) []byte {
	return appendProtoMessage(dst, number, []byte(value))
}

func appendProtoMessage(dst []byte, number protowire.Number, value []byte) []byte {
	dst = protowire.AppendTag(dst, number, protowire.BytesType)
	return protowire.AppendBytes(dst, value)
}
