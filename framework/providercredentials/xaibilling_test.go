package providercredentials

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestXAIUpdateAutoTopUpUsesGrokBillingRPCAndVerifies(t *testing.T) {
	var mu sync.Mutex
	enabled, threshold, amount, monthly := false, int64(500), int64(1000), int64(5000)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer xai-access", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case xAIAutoTopUpPath:
			mu.Lock()
			defer mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"rule":{"enabled":%t,"minBeforeHittingSl":{"val":%d},"topupAmount":{"val":%d},"maxAmountPerMonth":{"val":%d}}}`, enabled, threshold, amount, monthly)
		case xAISetAutoTopUpRPCPath:
			require.Equal(t, xAIWebRPCContentType, r.Header.Get("Content-Type"))
			payload := readXAIRequestFrame(t, r)
			rule := consumeBytesField(t, payload, 1)
			mu.Lock()
			enabled = consumeVarintField(t, rule, 1) != 0
			threshold = int64(consumeVarintField(t, consumeBytesField(t, rule, 2), 1))
			amount = int64(consumeVarintField(t, consumeBytesField(t, rule, 3), 1))
			monthly = int64(consumeVarintField(t, consumeBytesField(t, rule, 4), 1))
			mu.Unlock()
			_, _ = w.Write(grpcWebResponse(nil))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	manager, db := newTestManager(t, server)
	manager.endpoints.xAIUsageAPI = server.URL
	manager.endpoints.xAIWebAPI = server.URL
	expiresAt := time.Now().Add(time.Hour)
	require.NoError(t, db.Create(&tables.TableProviderCredential{CredentialID: "xai-key", Provider: string(ProviderXAI), ProviderKeyID: "xai-key", AuthMode: "device_code", AccessToken: "xai-access", RefreshToken: "xai-refresh", AccountID: "xai-user", ExpiresAt: &expiresAt, Status: StatusConnected, Version: 1}).Error)

	expectedEnabled := false
	expectedThreshold, expectedAmount, expectedMonthly := int32(5), int32(10), int32(50)
	updated, err := manager.UpdateAutoTopUp(context.Background(), ProviderXAI, "xai-key", UpdateCredentialAutoTopUpRequest{
		Enabled: true, ThresholdDollars: 7, TopUpAmountDollars: 20, MonthlyLimitDollars: 100,
		ExpectedEnabled: &expectedEnabled, ExpectedThresholdDollars: &expectedThreshold,
		ExpectedTopUpAmountDollars: &expectedAmount, ExpectedMonthlyLimitDollars: &expectedMonthly,
	})
	require.NoError(t, err)
	require.True(t, updated.Enabled)
	require.Equal(t, 700.0, *updated.Threshold)
	require.Equal(t, 2000.0, *updated.TopUpAmount)
	require.Equal(t, 10000.0, *updated.MonthlyLimit)
}

func TestXAIRedeemResetSendsTokenAndReturnsRemainingTokens(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	remaining := appendProtoString(nil, 10, "reset-2")
	remaining = appendProtoMessage(remaining, 30, timestampProto(now.Add(48*time.Hour)))
	response := appendProtoMessage(nil, 10, remaining)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, xAIRedeemResetRPCPath, r.URL.Path)
		require.Equal(t, "reset-1", string(consumeBytesField(t, readXAIRequestFrame(t, r), 10)))
		_, _ = w.Write(grpcWebResponse(response))
	}))
	defer server.Close()
	manager, db := newTestManager(t, server)
	manager.endpoints.xAIWebAPI = server.URL
	manager.now = func() time.Time { return now }
	expiresAt := now.Add(time.Hour)
	require.NoError(t, db.Create(&tables.TableProviderCredential{CredentialID: "xai-key", Provider: string(ProviderXAI), ProviderKeyID: "xai-key", AuthMode: "device_code", AccessToken: "xai-access", RefreshToken: "xai-refresh", AccountID: "xai-user", ExpiresAt: &expiresAt, Status: StatusConnected, Version: 1}).Error)

	credits, err := manager.RedeemReset(context.Background(), ProviderXAI, "xai-key", "reset-1")
	require.NoError(t, err)
	require.Equal(t, int64(1), credits.AvailableCount)
	require.Equal(t, "reset-2", credits.Credits[0].ID)
}

func readXAIRequestFrame(t *testing.T, r *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(body), 5)
	require.Zero(t, body[0])
	length := int(binary.BigEndian.Uint32(body[1:5]))
	require.Equal(t, len(body)-5, length)
	return body[5:]
}

func consumeBytesField(t *testing.T, body []byte, wanted protowire.Number) []byte {
	t.Helper()
	for len(body) > 0 {
		number, wireType, n := protowire.ConsumeTag(body)
		require.Positive(t, n)
		body = body[n:]
		if number == wanted {
			require.Equal(t, protowire.BytesType, wireType)
			value, consumed := protowire.ConsumeBytes(body)
			require.Positive(t, consumed)
			return value
		}
		consumed := protowire.ConsumeFieldValue(number, wireType, body)
		require.Positive(t, consumed)
		body = body[consumed:]
	}
	t.Fatalf("protobuf field %d was not present", wanted)
	return nil
}

func consumeVarintField(t *testing.T, body []byte, wanted protowire.Number) uint64 {
	t.Helper()
	for len(body) > 0 {
		number, wireType, n := protowire.ConsumeTag(body)
		require.Positive(t, n)
		body = body[n:]
		if number == wanted {
			require.Equal(t, protowire.VarintType, wireType)
			value, consumed := protowire.ConsumeVarint(body)
			require.Positive(t, consumed)
			return value
		}
		consumed := protowire.ConsumeFieldValue(number, wireType, body)
		require.Positive(t, consumed)
		body = body[consumed:]
	}
	t.Fatalf("protobuf field %d was not present", wanted)
	return 0
}
