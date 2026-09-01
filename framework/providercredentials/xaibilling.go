package providercredentials

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"google.golang.org/protobuf/encoding/protowire"
)

const (
	xAIAutoTopUpPath                   = "/auto-topup-rule"
	xAISetOnDemandRPCPath              = "/grok_api_v2.GrokBuildBilling/SetGrokBuildOnDemandConfig"
	xAISetAutoTopUpRPCPath             = "/grok_api_v2.GrokBuildBilling/SetAutoTopupRule"
	xAIGetRemainingResetsRPCPath       = "/prod_mc_billing.ConsumerUiSvc/GetRemainingResets"
	xAIRedeemResetRPCPath              = "/prod_mc_billing.ConsumerUiSvc/RedeemReset"
	xAIWebRPCContentType               = "application/grpc-web+proto"
	xAIWebRPCUnauthenticated     int64 = 16
)

var ErrResetUnsupportedProvider = errors.New("provider does not support usage reset redemption")

type UpdateCredentialAutoTopUpRequest struct {
	Enabled                     bool   `json:"enabled"`
	ThresholdDollars            int32  `json:"threshold_dollars"`
	TopUpAmountDollars          int32  `json:"top_up_amount_dollars"`
	MonthlyLimitDollars         int32  `json:"monthly_limit_dollars"`
	ExpectedEnabled             *bool  `json:"expected_enabled,omitempty"`
	ExpectedThresholdDollars    *int32 `json:"expected_threshold_dollars,omitempty"`
	ExpectedTopUpAmountDollars  *int32 `json:"expected_top_up_amount_dollars,omitempty"`
	ExpectedMonthlyLimitDollars *int32 `json:"expected_monthly_limit_dollars,omitempty"`
}

type xAIWebRPCError struct {
	code    int64
	message string
}

func (e *xAIWebRPCError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("xAI web RPC failed with code %d", e.code)
	}
	return fmt.Sprintf("xAI web RPC failed with code %d: %s", e.code, e.message)
}

func (e *xAIWebRPCError) unauthenticated() bool {
	return e.code == xAIWebRPCUnauthenticated
}

func (m *Manager) xAIAutoTopUp(ctx context.Context, credentialID string) (*CredentialAutoTopUp, error) {
	credential, err := m.resolveProviderCredentialForUsage(ctx, ProviderXAI, credentialID, false)
	if err != nil {
		return nil, err
	}
	request := func() (usageHTTPResponse, error) {
		requestCtx, cancel := context.WithTimeout(ctx, providerUsageTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, m.endpoints.xAIUsageAPI+xAIAutoTopUpPath, nil)
		if err != nil {
			return usageHTTPResponse{}, err
		}
		setXAIHeaders(req, credential)
		return m.doUsageRequest(req)
	}
	resp, err := request()
	if err == nil && (resp.status == http.StatusUnauthorized || resp.status == http.StatusForbidden) {
		credential, err = m.resolveProviderCredentialForUsage(ctx, ProviderXAI, credentialID, true)
		if err == nil {
			resp, err = request()
		}
	}
	if err != nil {
		return nil, err
	}
	if resp.status < http.StatusOK || resp.status >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("xAI auto top-up returned status %d", resp.status)
	}
	return parseXAIAutoTopUp(resp.body)
}

func parseXAIAutoTopUp(body []byte) (*CredentialAutoTopUp, error) {
	payload := struct {
		Rule *struct {
			Enabled      bool     `json:"enabled"`
			Threshold    *xAICent `json:"minBeforeHittingSl"`
			TopUpAmount  *xAICent `json:"topupAmount"`
			MonthlyLimit *xAICent `json:"maxAmountPerMonth"`
		} `json:"rule"`
	}{}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	autoTopUp := &CredentialAutoTopUp{CanUpdate: true, Unit: "cent"}
	if payload.Rule == nil {
		autoTopUp.DisabledReason = "Auto Top-Up is not configured"
		return autoTopUp, nil
	}
	autoTopUp.Enabled = payload.Rule.Enabled
	if payload.Rule.Threshold != nil {
		autoTopUp.Threshold = finiteFloat(&payload.Rule.Threshold.Val)
	}
	if payload.Rule.TopUpAmount != nil {
		autoTopUp.TopUpAmount = finiteFloat(&payload.Rule.TopUpAmount.Val)
	}
	if payload.Rule.MonthlyLimit != nil {
		autoTopUp.MonthlyLimit = finiteFloat(&payload.Rule.MonthlyLimit.Val)
	}
	if !autoTopUp.Enabled {
		autoTopUp.DisabledReason = "Auto Top-Up is disabled"
	}
	return autoTopUp, nil
}

func (m *Manager) UpdateAutoTopUp(
	ctx context.Context,
	provider schemas.ModelProvider,
	credentialID string,
	update UpdateCredentialAutoTopUpRequest,
) (*CredentialAutoTopUp, error) {
	if provider != ProviderXAI {
		return nil, ErrOnDemandUnsupportedProvider
	}
	if update.ThresholdDollars < 0 || update.TopUpAmountDollars < 0 || update.MonthlyLimitDollars < 0 {
		return nil, errors.New("auto top-up values cannot be negative")
	}
	if update.Enabled && (update.ThresholdDollars == 0 || update.TopUpAmountDollars == 0 || update.MonthlyLimitDollars == 0) {
		return nil, errors.New("auto top-up values must be positive when enabled")
	}
	current, err := m.xAIAutoTopUp(ctx, credentialID)
	if err != nil {
		return nil, err
	}
	if update.ExpectedEnabled != nil && *update.ExpectedEnabled != current.Enabled {
		return nil, ErrOnDemandConflict
	}
	if expectedDollarsChanged(update.ExpectedThresholdDollars, current.Threshold) ||
		expectedDollarsChanged(update.ExpectedTopUpAmountDollars, current.TopUpAmount) ||
		expectedDollarsChanged(update.ExpectedMonthlyLimitDollars, current.MonthlyLimit) {
		return nil, ErrOnDemandConflict
	}

	rule := appendXAIProtoVarint(nil, 1, boolVarint(update.Enabled))
	rule = appendXAIProtoMessage(rule, 2, encodeXAICent(int64(update.ThresholdDollars)*100))
	rule = appendXAIProtoMessage(rule, 3, encodeXAICent(int64(update.TopUpAmountDollars)*100))
	rule = appendXAIProtoMessage(rule, 4, encodeXAICent(int64(update.MonthlyLimitDollars)*100))
	if _, err := m.xAIWebRPCForCredential(ctx, credentialID, xAISetAutoTopUpRPCPath, appendXAIProtoMessage(nil, 1, rule)); err != nil {
		return nil, err
	}
	return m.verifyXAIAutoTopUp(ctx, credentialID, update)
}

func (m *Manager) updateXAIOnDemand(ctx context.Context, credentialID string, update UpdateCredentialOnDemandRequest) (*CredentialOnDemand, error) {
	if update.LimitDollars < 0 || (update.Enabled && update.LimitDollars == 0) {
		return nil, errors.New("on-demand limit must be a positive whole-dollar amount when enabled")
	}
	currentUsage := m.Usage(ctx, ProviderXAI, credentialID)
	current := currentUsage.OnDemand
	if current == nil || !current.CanUpdate {
		return nil, ErrOnDemandUnsupportedProvider
	}
	currentLimitDollars := int32(0)
	if current.Limit != nil {
		currentLimitDollars = int32(*current.Limit / 100)
	}
	if update.ExpectedEnabled != nil && *update.ExpectedEnabled != current.Enabled {
		return nil, ErrOnDemandConflict
	}
	if update.ExpectedLimitDollars != nil && *update.ExpectedLimitDollars != currentLimitDollars {
		return nil, ErrOnDemandConflict
	}
	capCents := int64(0)
	if update.Enabled {
		capCents = int64(update.LimitDollars) * 100
	}
	payload := appendXAIProtoMessage(nil, 1, encodeXAICent(capCents))
	if _, err := m.xAIWebRPCForCredential(ctx, credentialID, xAISetOnDemandRPCPath, payload); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 4; attempt++ {
		verified := m.Usage(ctx, ProviderXAI, credentialID).OnDemand
		if verified != nil && verified.Enabled == update.Enabled && (!update.Enabled || centsEqualDollars(verified.Limit, update.LimitDollars)) {
			return verified, nil
		}
		if attempt < 3 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	return nil, errors.New("xAI did not apply the requested on-demand settings")
}

func (m *Manager) verifyXAIAutoTopUp(ctx context.Context, credentialID string, update UpdateCredentialAutoTopUpRequest) (*CredentialAutoTopUp, error) {
	for attempt := 0; attempt < 4; attempt++ {
		verified, err := m.xAIAutoTopUp(ctx, credentialID)
		if err == nil && autoTopUpMatches(verified, update) {
			return verified, nil
		}
		if attempt < 3 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	return nil, errors.New("xAI did not apply the requested auto top-up settings")
}

func autoTopUpMatches(value *CredentialAutoTopUp, update UpdateCredentialAutoTopUpRequest) bool {
	return value != nil && value.Enabled == update.Enabled &&
		centsEqualDollars(value.Threshold, update.ThresholdDollars) &&
		centsEqualDollars(value.TopUpAmount, update.TopUpAmountDollars) &&
		centsEqualDollars(value.MonthlyLimit, update.MonthlyLimitDollars)
}

func expectedDollarsChanged(expected *int32, cents *float64) bool {
	return expected != nil && !centsEqualDollars(cents, *expected)
}

func centsEqualDollars(cents *float64, dollars int32) bool {
	if cents == nil {
		return dollars == 0
	}
	return *cents == float64(dollars)*100
}

func (m *Manager) xAIResetCredits(ctx context.Context, credentialID string) (*ResetCredits, error) {
	body, err := m.xAIWebRPCForCredential(ctx, credentialID, xAIGetRemainingResetsRPCPath, nil)
	if err != nil {
		return nil, err
	}
	return parseXAIResetCredits(body, m.now().UTC())
}

func (m *Manager) RedeemReset(ctx context.Context, provider schemas.ModelProvider, credentialID, resetID string) (*ResetCredits, error) {
	if provider != ProviderXAI {
		return nil, ErrResetUnsupportedProvider
	}
	resetID = strings.TrimSpace(resetID)
	if resetID == "" {
		return nil, errors.New("reset ID is required")
	}
	body, err := m.xAIWebRPCForCredential(ctx, credentialID, xAIRedeemResetRPCPath, appendXAIProtoString(nil, 10, resetID))
	if err != nil {
		return nil, err
	}
	return parseXAIResetCredits(body, m.now().UTC())
}

func parseXAIResetCredits(body []byte, now time.Time) (*ResetCredits, error) {
	credits := &ResetCredits{CanRedeem: true, Credits: make([]ResetCreditDetails, 0)}
	for len(body) > 0 {
		number, wireType, consumed := protowire.ConsumeTag(body)
		if consumed < 0 {
			return nil, protowire.ParseError(consumed)
		}
		body = body[consumed:]
		if number != 10 || wireType != protowire.BytesType {
			consumed = protowire.ConsumeFieldValue(number, wireType, body)
			if consumed < 0 {
				return nil, protowire.ParseError(consumed)
			}
			body = body[consumed:]
			continue
		}
		token, consumed := protowire.ConsumeBytes(body)
		if consumed < 0 {
			return nil, protowire.ParseError(consumed)
		}
		body = body[consumed:]
		credit, err := parseXAIResetToken(token)
		if err != nil {
			return nil, err
		}
		if credit.ID == "" || credit.ExpiresAt == nil || !credit.ExpiresAt.After(now) {
			continue
		}
		credits.Credits = append(credits.Credits, credit)
	}
	credits.AvailableCount = int64(len(credits.Credits))
	return credits, nil
}

func parseXAIResetToken(body []byte) (ResetCreditDetails, error) {
	credit := ResetCreditDetails{ResetType: "grok_usage_limit", Status: "available", Title: "Usage reset", Description: "Restores included Grok usage"}
	for len(body) > 0 {
		number, wireType, consumed := protowire.ConsumeTag(body)
		if consumed < 0 {
			return credit, protowire.ParseError(consumed)
		}
		body = body[consumed:]
		switch number {
		case 10:
			value, n := protowire.ConsumeString(body)
			if n < 0 {
				return credit, protowire.ParseError(n)
			}
			credit.ID = value
			body = body[n:]
		case 20, 30:
			value, n := protowire.ConsumeBytes(body)
			if n < 0 {
				return credit, protowire.ParseError(n)
			}
			timestamp, err := parseProtoTimestamp(value)
			if err != nil {
				return credit, err
			}
			if number == 20 {
				credit.GrantedAt = timestamp
			} else {
				credit.ExpiresAt = timestamp
			}
			body = body[n:]
		default:
			n := protowire.ConsumeFieldValue(number, wireType, body)
			if n < 0 {
				return credit, protowire.ParseError(n)
			}
			body = body[n:]
		}
	}
	return credit, nil
}

func parseProtoTimestamp(body []byte) (*time.Time, error) {
	var seconds int64
	var nanos int32
	for len(body) > 0 {
		number, wireType, consumed := protowire.ConsumeTag(body)
		if consumed < 0 {
			return nil, protowire.ParseError(consumed)
		}
		body = body[consumed:]
		if wireType != protowire.VarintType {
			consumed = protowire.ConsumeFieldValue(number, wireType, body)
			if consumed < 0 {
				return nil, protowire.ParseError(consumed)
			}
			body = body[consumed:]
			continue
		}
		value, n := protowire.ConsumeVarint(body)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		if number == 1 {
			seconds = int64(value)
		} else if number == 2 {
			nanos = int32(value)
		}
		body = body[n:]
	}
	value := time.Unix(seconds, int64(nanos)).UTC()
	return &value, nil
}

func (m *Manager) xAIWebRPCForCredential(ctx context.Context, credentialID, path string, payload []byte) ([]byte, error) {
	credential, err := m.resolveProviderCredentialForUsage(ctx, ProviderXAI, credentialID, false)
	if err != nil {
		return nil, err
	}
	response, err := m.doXAIWebRPC(ctx, credential, path, payload)
	var rpcErr *xAIWebRPCError
	if errors.As(err, &rpcErr) && rpcErr.unauthenticated() {
		credential, err = m.resolveProviderCredentialForUsage(ctx, ProviderXAI, credentialID, true)
		if err != nil {
			return nil, err
		}
		return m.doXAIWebRPC(ctx, credential, path, payload)
	}
	return response, err
}

func (m *Manager) doXAIWebRPC(ctx context.Context, credential schemas.ResolvedProviderCredential, path string, payload []byte) ([]byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, providerUsageTimeout)
	defer cancel()
	framed := make([]byte, 5, 5+len(payload))
	binary.BigEndian.PutUint32(framed[1:], uint32(len(payload)))
	framed = append(framed, payload...)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, m.endpoints.xAIWebAPI+path, bytes.NewReader(framed))
	if err != nil {
		return nil, err
	}
	setXAIHeaders(req, credential)
	req.Header.Set("Content-Type", xAIWebRPCContentType)
	req.Header.Set("Accept", xAIWebRPCContentType)
	req.Header.Set("X-Grpc-Web", "1")
	req.Header.Set("TE", "trailers")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // Best-effort connection cleanup.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUsageResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxUsageResponseBytes {
		return nil, errors.New("xAI web RPC response is too large")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, &xAIWebRPCError{code: xAIWebRPCUnauthenticated, message: fmt.Sprintf("HTTP %d", resp.StatusCode)}
		}
		return nil, fmt.Errorf("xAI web RPC returned status %d", resp.StatusCode)
	}
	if grpcStatus := strings.TrimSpace(resp.Header.Get("Grpc-Status")); grpcStatus != "" && grpcStatus != "0" {
		return nil, newXAIWebRPCError(grpcStatus, resp.Header.Get("Grpc-Message"))
	}
	return parseXAIWebRPCFrames(body)
}

func parseXAIWebRPCFrames(body []byte) ([]byte, error) {
	var message []byte
	for len(body) > 0 {
		if len(body) < 5 {
			return nil, errors.New("xAI web RPC returned a truncated frame")
		}
		flags := body[0]
		length := int(binary.BigEndian.Uint32(body[1:5]))
		body = body[5:]
		if length > len(body) {
			return nil, errors.New("xAI web RPC returned an invalid frame length")
		}
		frame := body[:length]
		body = body[length:]
		if flags&0x80 != 0 {
			trailers := parseXAIWebRPCTrailers(frame)
			if status := trailers["grpc-status"]; status != "" && status != "0" {
				return nil, newXAIWebRPCError(status, trailers["grpc-message"])
			}
			continue
		}
		if flags != 0 {
			return nil, errors.New("xAI web RPC returned unsupported frame flags")
		}
		if message != nil {
			return nil, errors.New("xAI web RPC returned multiple response messages")
		}
		message = append([]byte(nil), frame...)
	}
	if message == nil {
		return []byte{}, nil
	}
	return message, nil
}

func parseXAIWebRPCTrailers(frame []byte) map[string]string {
	trailers := make(map[string]string)
	for _, line := range strings.Split(string(frame), "\r\n") {
		name, value, ok := strings.Cut(line, ":")
		if ok {
			trailers[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
		}
	}
	return trailers
}

func newXAIWebRPCError(status, message string) error {
	code, _ := strconv.ParseInt(status, 10, 64)
	if decoded, err := url.QueryUnescape(message); err == nil {
		message = decoded
	}
	return &xAIWebRPCError{code: code, message: message}
}

func setXAIHeaders(req *http.Request, credential schemas.ResolvedProviderCredential) {
	req.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("x-grok-client-version", xAIClientVersion)
	req.Header.Set("x-grok-client-mode", "headless")
	userID := strings.TrimSpace(credential.AccountID)
	if userID == "" {
		userID = jwtSubject(credential.AccessToken)
	}
	if userID != "" {
		req.Header.Set("x-userid", userID)
	}
}

func encodeXAICent(cents int64) []byte {
	return appendXAIProtoVarint(nil, 1, uint64(cents))
}

func appendXAIProtoVarint(message []byte, field protowire.Number, value uint64) []byte {
	message = protowire.AppendTag(message, field, protowire.VarintType)
	return protowire.AppendVarint(message, value)
}

func appendXAIProtoMessage(message []byte, field protowire.Number, value []byte) []byte {
	message = protowire.AppendTag(message, field, protowire.BytesType)
	return protowire.AppendBytes(message, value)
}

func appendXAIProtoString(message []byte, field protowire.Number, value string) []byte {
	message = protowire.AppendTag(message, field, protowire.BytesType)
	return protowire.AppendString(message, value)
}

func boolVarint(value bool) uint64 {
	if value {
		return 1
	}
	return 0
}
