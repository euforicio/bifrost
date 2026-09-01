package providercredentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/schemas"
	"google.golang.org/protobuf/encoding/protowire"
)

const (
	UsageAvailable   = "available"
	UsageUnsupported = "unsupported"
	UsageUnavailable = "unavailable"

	openAIUsagePath              = "/wham/usage"
	openAIResetCreditsPath       = "/wham/rate-limit-reset-credits"
	cursorCurrentPeriodUsagePath = "/aiserver.v1.DashboardService/GetCurrentPeriodUsage"
	cursorPlanInfoPath           = "/aiserver.v1.DashboardService/GetPlanInfo"
	cursorSandUsageStatusPath    = "/aiserver.v1.DashboardService/GetSandUsageStatus"
	cursorHardLimitPath          = "/aiserver.v1.DashboardService/GetHardLimit"
	xAIUserPath                  = "/user?include=subscription"
	xAIUsagePath                 = "/billing?format=credits"
	xAIClientVersion             = "1.0.13"
	maxUsageResponseBytes        = 1024 * 1024
	providerUsageTimeout         = 10 * time.Second
	resetCreditsTimeout          = 5 * time.Second
	usageUnavailableMessage      = "Provider usage is temporarily unavailable."
)

// CredentialUsage is the provider-neutral quota snapshot returned by the
// management API. Usage reads never alter credential or inference status.
type CredentialUsage struct {
	CredentialID string              `json:"credential_id"`
	Provider     string              `json:"provider"`
	Availability string              `json:"availability"`
	FetchedAt    *time.Time          `json:"fetched_at,omitempty"`
	Stale        *bool               `json:"stale,omitempty"`
	Message      string              `json:"message,omitempty"`
	Quotas       []CredentialQuota   `json:"quotas"`
	Plan         *CredentialPlan     `json:"plan,omitempty"`
	OnDemand     *CredentialOnDemand `json:"on_demand,omitempty"`
	Credits      *CredentialCredits  `json:"credits,omitempty"`
	ResetCredits *ResetCredits       `json:"reset_credits,omitempty"`
}

type CredentialQuota struct {
	ID                    string     `json:"id"`
	Name                  string     `json:"name"`
	Description           string     `json:"description,omitempty"`
	UsedPercent           *float64   `json:"used_percent,omitempty"`
	Used                  *float64   `json:"used,omitempty"`
	Limit                 *float64   `json:"limit,omitempty"`
	Remaining             *float64   `json:"remaining,omitempty"`
	Unit                  string     `json:"unit,omitempty"`
	WindowDurationMinutes *int64     `json:"window_duration_minutes,omitempty"`
	StartsAt              *time.Time `json:"starts_at,omitempty"`
	ResetsAt              *time.Time `json:"resets_at,omitempty"`
}

type CredentialPlan struct {
	Name            string     `json:"name"`
	Price           string     `json:"price,omitempty"`
	IncludedAmount  *float64   `json:"included_amount,omitempty"`
	IncludedUnit    string     `json:"included_unit,omitempty"`
	BillingCycleEnd *time.Time `json:"billing_cycle_end,omitempty"`
}

type CredentialOnDemand struct {
	Enabled        bool     `json:"enabled"`
	CanUpdate      bool     `json:"can_update"`
	Used           *float64 `json:"used,omitempty"`
	Limit          *float64 `json:"limit,omitempty"`
	Remaining      *float64 `json:"remaining,omitempty"`
	Unit           string   `json:"unit,omitempty"`
	LimitType      string   `json:"limit_type,omitempty"`
	DisabledReason string   `json:"disabled_reason,omitempty"`
}

type CredentialCredits struct {
	HasCredits bool     `json:"has_credits"`
	Unlimited  bool     `json:"unlimited"`
	Balance    *float64 `json:"balance,omitempty"`
	Unit       string   `json:"unit,omitempty"`
}

type ResetCredits struct {
	AvailableCount int64                `json:"available_count"`
	Credits        []ResetCreditDetails `json:"credits"`
}

type ResetCreditDetails struct {
	ID          string     `json:"id"`
	ResetType   string     `json:"reset_type"`
	Status      string     `json:"status"`
	GrantedAt   *time.Time `json:"granted_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	Title       string     `json:"title,omitempty"`
	Description string     `json:"description,omitempty"`
}

// Usage fetches current quota state for one stored provider credential. An
// upstream usage failure is represented as unavailable rather than mutating or
// disconnecting the credential used by inference.
func (m *Manager) Usage(ctx context.Context, provider schemas.ModelProvider, credentialID string) CredentialUsage {
	base := CredentialUsage{
		CredentialID: credentialID,
		Provider:     string(provider),
		Quotas:       make([]CredentialQuota, 0),
	}
	if provider != ProviderOpenAICodex && provider != ProviderXAI && provider != ProviderCursor {
		base.Availability = UsageUnsupported
		base.Message = "Provider account usage is not supported."
		return base
	}
	credential, err := m.resolveProviderCredentialForUsage(ctx, provider, credentialID, false)
	if err != nil {
		base.Availability = UsageUnavailable
		base.Message = usageUnavailableMessage
		return base
	}
	var usage CredentialUsage
	switch provider {
	case ProviderOpenAICodex:
		usage, err = m.openAIUsage(ctx, credentialID, credential)
	case ProviderXAI:
		usage, err = m.xAIUsage(ctx, credentialID, credential)
	case ProviderCursor:
		usage, err = m.cursorUsage(ctx, credentialID, credential)
	}
	if err != nil {
		base.Availability = UsageUnavailable
		base.Message = usageUnavailableMessage
		return base
	}
	return usage
}

type xAIUserPayload struct {
	UserID           string `json:"userId"`
	SubscriptionTier string `json:"subscriptionTier"`
}

type xAICent struct {
	Val float64 `json:"val"`
}

type xAIUsagePeriod struct {
	Type  string `json:"type"`
	Start string `json:"start"`
	End   string `json:"end"`
}

type xAIProductUsage struct {
	Product      string   `json:"product"`
	UsagePercent *float64 `json:"usagePercent"`
}

type xAIUsagePayload struct {
	Config *struct {
		CreditUsagePercent *float64          `json:"creditUsagePercent"`
		CurrentPeriod      *xAIUsagePeriod   `json:"currentPeriod"`
		MonthlyLimit       *xAICent          `json:"monthlyLimit"`
		Used               *xAICent          `json:"used"`
		OnDemandCap        *xAICent          `json:"onDemandCap"`
		OnDemandUsed       *xAICent          `json:"onDemandUsed"`
		PrepaidBalance     *xAICent          `json:"prepaidBalance"`
		ProductUsage       []xAIProductUsage `json:"productUsage"`
		UnifiedBilling     *bool             `json:"isUnifiedBillingUser"`
		BillingPeriodStart string            `json:"billingPeriodStart"`
		BillingPeriodEnd   string            `json:"billingPeriodEnd"`
	} `json:"config"`
}

func (m *Manager) xAIUsage(ctx context.Context, credentialID string, credential schemas.ResolvedProviderCredential) (CredentialUsage, error) {
	forcedRefresh := false
	request := func(path string, userID string) (usageHTTPResponse, error) {
		requestCtx, cancel := context.WithTimeout(ctx, providerUsageTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, m.endpoints.xAIUsageAPI+path, nil)
		if err != nil {
			return usageHTTPResponse{}, err
		}
		req.Header.Set("Authorization", "Bearer "+credential.AccessToken)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
		req.Header.Set("x-grok-client-version", xAIClientVersion)
		req.Header.Set("x-grok-client-mode", "headless")
		if userID != "" {
			req.Header.Set("x-userid", userID)
		}
		return m.doUsageRequest(req)
	}
	fetch := func(path string, userID string, retryUnauthorized bool) (usageHTTPResponse, error) {
		resp, err := request(path, userID)
		if err != nil || resp.status != http.StatusUnauthorized || !retryUnauthorized || forcedRefresh {
			return resp, err
		}
		forcedRefresh = true
		credential, err = m.resolveProviderCredentialForUsage(ctx, ProviderXAI, credentialID, true)
		if err != nil {
			return usageHTTPResponse{}, err
		}
		return request(path, userID)
	}

	userID := strings.TrimSpace(credential.AccountID)
	if userID == "" {
		userID = jwtSubject(credential.AccessToken)
	}
	var user xAIUserPayload
	userResp, userErr := fetch(xAIUserPath, "", true)
	if userErr == nil && userResp.status >= http.StatusOK && userResp.status < http.StatusMultipleChoices {
		if err := json.Unmarshal(userResp.body, &user); err == nil && strings.TrimSpace(user.UserID) != "" {
			userID = strings.TrimSpace(user.UserID)
		}
	}
	if userID == "" {
		if userErr != nil {
			return CredentialUsage{}, userErr
		}
		return CredentialUsage{}, fmt.Errorf("xAI user identity returned status %d", userResp.status)
	}

	resp, err := fetch(xAIUsagePath, userID, true)
	if err != nil {
		return CredentialUsage{}, err
	}
	if resp.status < http.StatusOK || resp.status >= http.StatusMultipleChoices {
		return CredentialUsage{}, fmt.Errorf("xAI usage returned status %d", resp.status)
	}
	return parseXAIUsage(resp.body, credentialID, user.SubscriptionTier, m.now().UTC())
}

func jwtSubject(token string) string {
	claims := jwtClaims(token)
	subject, _ := claims["sub"].(string)
	return strings.TrimSpace(subject)
}

func parseXAIUsage(body []byte, credentialID, subscriptionTier string, fetchedAt time.Time) (CredentialUsage, error) {
	var payload xAIUsagePayload
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return CredentialUsage{}, err
	}
	if payload.Config == nil {
		return CredentialUsage{}, errors.New("xAI usage response did not include a billing config")
	}
	config := payload.Config
	usage := CredentialUsage{
		CredentialID: credentialID,
		Provider:     string(ProviderXAI),
		Availability: UsageAvailable,
		FetchedAt:    &fetchedAt,
		Quotas:       make([]CredentialQuota, 0, 1+len(config.ProductUsage)),
	}
	quota := CredentialQuota{ID: "grok-subscription", Name: "Grok subscription", UsedPercent: finiteFloat(config.CreditUsagePercent)}
	if config.MonthlyLimit != nil {
		quota.Limit = finiteFloat(&config.MonthlyLimit.Val)
		quota.Unit = "cent"
	}
	if config.Used != nil {
		quota.Used = finiteFloat(&config.Used.Val)
		quota.Unit = "cent"
	}
	if quota.Limit != nil && quota.Used != nil && *quota.Limit > 0 {
		remaining := math.Max(0, *quota.Limit-*quota.Used)
		quota.Remaining = &remaining
		if quota.UsedPercent == nil {
			percent := math.Min(100, math.Max(0, *quota.Used / *quota.Limit * 100))
			quota.UsedPercent = &percent
		}
	}
	applyXAIUsagePeriod(&quota, config.CurrentPeriod, config.BillingPeriodStart, config.BillingPeriodEnd)
	usage.Quotas = append(usage.Quotas, quota)
	for _, product := range config.ProductUsage {
		name := strings.TrimSpace(product.Product)
		if name == "" || product.UsagePercent == nil {
			continue
		}
		usage.Quotas = append(usage.Quotas, CredentialQuota{
			ID:                    "grok-product:" + safeUsageID(name),
			Name:                  name,
			Description:           "Included product usage",
			UsedPercent:           finiteFloat(product.UsagePercent),
			StartsAt:              quota.StartsAt,
			ResetsAt:              quota.ResetsAt,
			WindowDurationMinutes: quota.WindowDurationMinutes,
		})
	}
	if tier := strings.TrimSpace(subscriptionTier); tier != "" {
		usage.Plan = &CredentialPlan{Name: tier, BillingCycleEnd: quota.ResetsAt}
	}
	if config.OnDemandCap != nil || config.OnDemandUsed != nil {
		onDemand := &CredentialOnDemand{CanUpdate: false, Unit: "cent", LimitType: "provider_managed"}
		if config.OnDemandCap != nil {
			onDemand.Limit = finiteFloat(&config.OnDemandCap.Val)
			onDemand.Enabled = config.OnDemandCap.Val > 0
		}
		if config.OnDemandUsed != nil {
			onDemand.Used = finiteFloat(&config.OnDemandUsed.Val)
		}
		if onDemand.Limit != nil && onDemand.Used != nil {
			remaining := math.Max(0, *onDemand.Limit-*onDemand.Used)
			onDemand.Remaining = &remaining
		}
		if !onDemand.Enabled {
			onDemand.DisabledReason = "On-demand spending is disabled in Grok"
		}
		usage.OnDemand = onDemand
	}
	if config.PrepaidBalance != nil {
		balance := config.PrepaidBalance.Val
		usage.Credits = &CredentialCredits{HasCredits: balance > 0, Balance: finiteFloat(&balance), Unit: "cent"}
	}
	if config.UnifiedBilling != nil && *config.UnifiedBilling {
		usage.Message = "Usage is shared across the account's unified Grok billing pool."
	}
	return usage, nil
}

func applyXAIUsagePeriod(quota *CredentialQuota, period *xAIUsagePeriod, legacyStart, legacyEnd string) {
	startText, endText, periodType := legacyStart, legacyEnd, ""
	if period != nil {
		periodType = period.Type
		if period.Start != "" {
			startText = period.Start
		}
		if period.End != "" {
			endText = period.End
		}
	}
	if started, err := time.Parse(time.RFC3339, startText); err == nil {
		started = started.UTC()
		quota.StartsAt = &started
	}
	if reset, err := time.Parse(time.RFC3339, endText); err == nil {
		reset = reset.UTC()
		quota.ResetsAt = &reset
	}
	if quota.StartsAt != nil && quota.ResetsAt != nil && quota.ResetsAt.After(*quota.StartsAt) {
		minutes := int64(quota.ResetsAt.Sub(*quota.StartsAt) / time.Minute)
		quota.WindowDurationMinutes = &minutes
	}
	if strings.Contains(strings.ToUpper(periodType), "WEEKLY") {
		quota.Description = "Weekly included usage"
	} else if strings.Contains(strings.ToUpper(periodType), "MONTHLY") {
		quota.Description = "Monthly included usage"
	}
}

func safeUsageID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, value)
}

type usageHTTPResponse struct {
	status int
	body   []byte
}

func (m *Manager) openAIUsage(ctx context.Context, credentialID string, credential schemas.ResolvedProviderCredential) (CredentialUsage, error) {
	forcedRefresh := false
	request := func(path string, timeout time.Duration) (usageHTTPResponse, error) {
		requestCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, m.endpoints.openAIUsageAPI+path, nil)
		if err != nil {
			return usageHTTPResponse{}, err
		}
		req.Header.Set("Authorization", "Bearer "+credential.AccessToken)
		req.Header.Set("Accept", "application/json")
		if credential.AccountID != "" {
			req.Header.Set("ChatGPT-Account-ID", credential.AccountID)
		}
		return m.doUsageRequest(req)
	}
	fetch := func(path string, timeout time.Duration, retryUnauthorized bool) (usageHTTPResponse, error) {
		resp, err := request(path, timeout)
		if err != nil || resp.status != http.StatusUnauthorized || !retryUnauthorized || forcedRefresh {
			return resp, err
		}
		forcedRefresh = true
		credential, err = m.resolveProviderCredentialForUsage(ctx, ProviderOpenAICodex, credentialID, true)
		if err != nil {
			return usageHTTPResponse{}, err
		}
		return request(path, timeout)
	}

	resp, err := fetch(openAIUsagePath, providerUsageTimeout, true)
	if err != nil {
		return CredentialUsage{}, err
	}
	if resp.status < http.StatusOK || resp.status >= http.StatusMultipleChoices {
		return CredentialUsage{}, fmt.Errorf("openAI usage returned status %d", resp.status)
	}
	now := m.now().UTC()
	usage, err := parseOpenAIUsage(resp.body, credentialID, now)
	if err != nil {
		return CredentialUsage{}, err
	}

	// Reset-credit details are additive and best effort. A bounded failure does
	// not discard the usage snapshot or its summary count.
	details, detailErr := fetch(openAIResetCreditsPath, resetCreditsTimeout, true)
	if detailErr == nil && details.status >= http.StatusOK && details.status < http.StatusMultipleChoices {
		if parsed, parseErr := parseResetCreditDetails(details.body); parseErr == nil {
			usage.ResetCredits = parsed
		}
	}
	return usage, nil
}

func (m *Manager) cursorUsage(ctx context.Context, credentialID string, credential schemas.ResolvedProviderCredential) (CredentialUsage, error) {
	forcedRefresh := false
	request := func(path string) (usageHTTPResponse, error) {
		requestCtx, cancel := context.WithTimeout(ctx, providerUsageTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, m.endpoints.cursorAPIBase+path, bytes.NewReader(nil))
		if err != nil {
			return usageHTTPResponse{}, err
		}
		req.Header.Set("Authorization", "Bearer "+credential.AccessToken)
		req.Header.Set("Content-Type", "application/proto")
		req.Header.Set("Connect-Protocol-Version", "1")
		req.Header.Set("X-Cursor-Client-Type", "cli")
		req.Header.Set("X-Cursor-Client-Version", "cli-bifrost-1.0")
		req.Header.Set("X-Ghost-Mode", "true")
		req.Header.Set("X-Request-ID", uuid.NewString())
		return m.doUsageRequest(req)
	}
	fetch := func(path string, retryUnauthorized bool) (usageHTTPResponse, error) {
		resp, err := request(path)
		if err != nil || (resp.status != http.StatusUnauthorized && resp.status != http.StatusForbidden) || !retryUnauthorized || forcedRefresh {
			return resp, err
		}
		forcedRefresh = true
		credential, err = m.resolveProviderCredentialForUsage(ctx, ProviderCursor, credentialID, true)
		if err != nil {
			return usageHTTPResponse{}, err
		}
		return request(path)
	}
	resp, err := fetch(cursorCurrentPeriodUsagePath, true)
	if err != nil {
		return CredentialUsage{}, err
	}
	if resp.status < http.StatusOK || resp.status >= http.StatusMultipleChoices {
		return CredentialUsage{}, fmt.Errorf("cursor usage returned status %d", resp.status)
	}
	now := m.now().UTC()
	usage, err := parseCursorUsage(resp.body, credentialID, now)
	if err != nil {
		return CredentialUsage{}, err
	}
	if planResp, planErr := fetch(cursorPlanInfoPath, true); planErr == nil && planResp.status >= http.StatusOK && planResp.status < http.StatusMultipleChoices {
		if plan, parseErr := parseCursorPlanInfo(planResp.body); parseErr == nil {
			if plan != nil && plan.BillingCycleEnd == nil && len(usage.Quotas) > 0 {
				plan.BillingCycleEnd = usage.Quotas[0].ResetsAt
			}
			usage.Plan = plan
		}
	}
	if sandResp, sandErr := fetch(cursorSandUsageStatusPath, true); sandErr == nil && sandResp.status >= http.StatusOK && sandResp.status < http.StatusMultipleChoices {
		if quota, resets, parseErr := parseCursorSandUsage(sandResp.body); parseErr == nil {
			if quota != nil {
				usage.Quotas = append(usage.Quotas, *quota)
			}
			if resets != nil {
				usage.ResetCredits = resets
			}
		}
	}
	if hardLimitResp, hardLimitErr := fetch(cursorHardLimitPath, true); hardLimitErr == nil && hardLimitResp.status >= http.StatusOK && hardLimitResp.status < http.StatusMultipleChoices {
		if policy, parseErr := parseCursorHardLimit(hardLimitResp.body, usage.OnDemand); parseErr == nil {
			usage.OnDemand = policy
		}
	}
	return usage, nil
}

func (m *Manager) doUsageRequest(req *http.Request) (usageHTTPResponse, error) {
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return usageHTTPResponse{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // Best-effort connection cleanup.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUsageResponseBytes+1))
	if err != nil {
		return usageHTTPResponse{}, err
	}
	if len(body) > maxUsageResponseBytes {
		return usageHTTPResponse{}, errors.New("provider usage response is too large")
	}
	return usageHTTPResponse{status: resp.StatusCode, body: body}, nil
}

type openAIUsagePayload struct {
	RateLimit struct {
		PrimaryWindow   *openAIUsageWindow `json:"primary_window"`
		SecondaryWindow *openAIUsageWindow `json:"secondary_window"`
	} `json:"rate_limit"`
	AdditionalRateLimits []struct {
		MeteredFeature string `json:"metered_feature"`
		LimitName      string `json:"limit_name"`
		RateLimit      struct {
			PrimaryWindow   *openAIUsageWindow `json:"primary_window"`
			SecondaryWindow *openAIUsageWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	} `json:"additional_rate_limits"`
	Credits *struct {
		HasCredits bool            `json:"has_credits"`
		Unlimited  bool            `json:"unlimited"`
		Balance    json.RawMessage `json:"balance"`
	} `json:"credits"`
	ResetCredits *struct {
		AvailableCount int64 `json:"available_count"`
	} `json:"rate_limit_reset_credits"`
}

type openAIUsageWindow struct {
	UsedPercent       *float64 `json:"used_percent"`
	LimitWindowSecond int64    `json:"limit_window_seconds"`
	ResetAt           int64    `json:"reset_at"`
}

func parseOpenAIUsage(body []byte, credentialID string, fetchedAt time.Time) (CredentialUsage, error) {
	var payload openAIUsagePayload
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return CredentialUsage{}, err
	}
	usage := CredentialUsage{
		CredentialID: credentialID,
		Provider:     string(ProviderOpenAICodex),
		Availability: UsageAvailable,
		FetchedAt:    &fetchedAt,
		Quotas:       make([]CredentialQuota, 0, 2+len(payload.AdditionalRateLimits)*2),
	}
	usage.Quotas = appendOpenAIWindows(usage.Quotas, "codex", "Codex", payload.RateLimit.PrimaryWindow, payload.RateLimit.SecondaryWindow)
	for _, additional := range payload.AdditionalRateLimits {
		id := strings.TrimSpace(additional.MeteredFeature)
		if id == "" {
			id = "additional"
		}
		name := strings.TrimSpace(additional.LimitName)
		if name == "" {
			name = id
		}
		usage.Quotas = appendOpenAIWindows(usage.Quotas, id, name, additional.RateLimit.PrimaryWindow, additional.RateLimit.SecondaryWindow)
	}
	if payload.Credits != nil {
		usage.Credits = &CredentialCredits{
			HasCredits: payload.Credits.HasCredits,
			Unlimited:  payload.Credits.Unlimited,
			Balance:    rawJSONNumber(payload.Credits.Balance),
		}
	}
	if payload.ResetCredits != nil {
		usage.ResetCredits = &ResetCredits{AvailableCount: payload.ResetCredits.AvailableCount, Credits: make([]ResetCreditDetails, 0)}
	}
	return usage, nil
}

func appendOpenAIWindows(quotas []CredentialQuota, id, name string, primary, secondary *openAIUsageWindow) []CredentialQuota {
	if primary != nil {
		quotas = append(quotas, openAIQuota(id+":primary", name+" primary window", primary))
	}
	if secondary != nil {
		quotas = append(quotas, openAIQuota(id+":secondary", name+" secondary window", secondary))
	}
	return quotas
}

func openAIQuota(id, name string, window *openAIUsageWindow) CredentialQuota {
	quota := CredentialQuota{ID: id, Name: name, UsedPercent: finiteFloat(window.UsedPercent)}
	if window.LimitWindowSecond > 0 {
		minutes := (window.LimitWindowSecond + 59) / 60
		quota.WindowDurationMinutes = &minutes
	}
	if window.ResetAt > 0 {
		reset := time.Unix(window.ResetAt, 0).UTC()
		quota.ResetsAt = &reset
		if window.LimitWindowSecond > 0 {
			start := reset.Add(-time.Duration(window.LimitWindowSecond) * time.Second)
			quota.StartsAt = &start
		}
	}
	return quota
}

func rawJSONNumber(raw json.RawMessage) *float64 {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		text = strings.TrimSpace(string(raw))
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return &value
}

func parseResetCreditDetails(body []byte) (*ResetCredits, error) {
	var payload struct {
		AvailableCount int64 `json:"available_count"`
		Credits        []struct {
			ID          string  `json:"id"`
			ResetType   string  `json:"reset_type"`
			Status      string  `json:"status"`
			GrantedAt   string  `json:"granted_at"`
			ExpiresAt   *string `json:"expires_at"`
			Title       *string `json:"title"`
			Description *string `json:"description"`
		} `json:"credits"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	out := &ResetCredits{AvailableCount: payload.AvailableCount, Credits: make([]ResetCreditDetails, 0, len(payload.Credits))}
	for _, raw := range payload.Credits {
		credit := ResetCreditDetails{ID: raw.ID, ResetType: raw.ResetType, Status: raw.Status}
		if raw.GrantedAt != "" {
			granted, err := time.Parse(time.RFC3339, raw.GrantedAt)
			if err != nil {
				return nil, err
			}
			granted = granted.UTC()
			credit.GrantedAt = &granted
		}
		if raw.ExpiresAt != nil && *raw.ExpiresAt != "" {
			expires, err := time.Parse(time.RFC3339, *raw.ExpiresAt)
			if err != nil {
				return nil, err
			}
			expires = expires.UTC()
			credit.ExpiresAt = &expires
		}
		if raw.Title != nil {
			credit.Title = *raw.Title
		}
		if raw.Description != nil {
			credit.Description = *raw.Description
		}
		out.Credits = append(out.Credits, credit)
	}
	return out, nil
}

type cursorPeriodUsage struct {
	billingStart     time.Time
	billingEnd       time.Time
	enabled          bool
	displayMessage   string
	autoBucketModels []string
	plan             *cursorPlanUsage
	spend            *cursorSpendLimitUsage
}

type cursorPlanUsage struct {
	totalSpend, includedSpend, bonusSpend, remaining, limit float64
	autoSpend, apiSpend, autoLimit, apiLimit                *float64
	autoPercentUsed, apiPercentUsed, totalPercentUsed       *float64
}

type cursorSpendLimitUsage struct {
	totalSpend                                  float64
	pooledLimit, pooledUsed, pooledRemaining    *float64
	individualLimit                             *float64
	individualUsed, individualRemaining         float64
	limitType                                   string
	overallLimit, overallUsed, overallRemaining *float64
}

func parseCursorUsage(body []byte, credentialID string, fetchedAt time.Time) (CredentialUsage, error) {
	period, err := decodeCursorPeriodUsage(body)
	if err != nil {
		return CredentialUsage{}, err
	}
	usage := CredentialUsage{
		CredentialID: credentialID,
		Provider:     string(ProviderCursor),
		Availability: UsageAvailable,
		FetchedAt:    &fetchedAt,
		Quotas:       make([]CredentialQuota, 0, 7),
	}
	if period.plan == nil {
		return CredentialUsage{}, errors.New("cursor usage did not include plan usage")
	}
	if period.plan != nil {
		usedPercent := period.plan.totalPercentUsed
		if usedPercent == nil && period.plan.limit > 0 {
			derived := period.plan.includedSpend / period.plan.limit * 100
			usedPercent = finiteFloat(&derived)
		}
		usage.Quotas = append(usage.Quotas, cursorQuota("plan", "Plan usage", &period.plan.includedSpend, &period.plan.limit, &period.plan.remaining, usedPercent, period))
		if period.plan.autoSpend != nil || period.plan.autoLimit != nil || period.plan.autoPercentUsed != nil {
			quota := cursorQuota("cursor-models", "Cursor Models", period.plan.autoSpend, period.plan.autoLimit, remaining(period.plan.autoLimit, period.plan.autoSpend), period.plan.autoPercentUsed, period)
			quota.Description = cursorModelsDescription(period.autoBucketModels)
			usage.Quotas = append(usage.Quotas, quota)
		}
		if period.plan.apiSpend != nil || period.plan.apiLimit != nil || period.plan.apiPercentUsed != nil {
			usage.Quotas = append(usage.Quotas, cursorQuota("other-models", "Other Models", period.plan.apiSpend, period.plan.apiLimit, remaining(period.plan.apiLimit, period.plan.apiSpend), period.plan.apiPercentUsed, period))
		}
	}
	if period.spend != nil {
		usage.OnDemand = &CredentialOnDemand{
			Enabled:   period.enabled,
			Used:      &period.spend.individualUsed,
			Limit:     period.spend.individualLimit,
			Remaining: &period.spend.individualRemaining,
			Unit:      "cents",
			LimitType: period.spend.limitType,
		}
		if period.displayMessage != "" {
			usage.OnDemand.DisabledReason = period.displayMessage
		}
		if period.spend.pooledLimit != nil || period.spend.pooledUsed != nil || period.spend.pooledRemaining != nil {
			usage.Quotas = append(usage.Quotas, cursorQuota("spend-limit:pooled", "Pooled spend limit", period.spend.pooledUsed, period.spend.pooledLimit, period.spend.pooledRemaining, nil, period))
		}
		if period.spend.overallLimit != nil || period.spend.overallUsed != nil || period.spend.overallRemaining != nil {
			usage.Quotas = append(usage.Quotas, cursorQuota("spend-limit:overall", "Overall spend limit", period.spend.overallUsed, period.spend.overallLimit, period.spend.overallRemaining, nil, period))
		}
	}
	return usage, nil
}

func cursorModelsDescription(models []string) string {
	var includesGrok, includesComposer bool
	for _, model := range models {
		normalized := strings.ToLower(model)
		includesGrok = includesGrok || strings.Contains(normalized, "grok")
		includesComposer = includesComposer || strings.Contains(normalized, "composer")
	}
	switch {
	case includesGrok && includesComposer:
		return "Includes Cursor Grok and Composer"
	case includesGrok:
		return "Includes Cursor Grok"
	case includesComposer:
		return "Includes Composer"
	default:
		return ""
	}
}

func cursorQuota(id, name string, used, limit, remainingValue, usedPercent *float64, period cursorPeriodUsage) CredentialQuota {
	quota := CredentialQuota{ID: id, Name: name, Used: used, Limit: limit, Remaining: remainingValue, UsedPercent: usedPercent, Unit: "cents"}
	if !period.billingStart.IsZero() {
		start := period.billingStart
		quota.StartsAt = &start
	}
	if !period.billingEnd.IsZero() {
		end := period.billingEnd
		quota.ResetsAt = &end
	}
	if !period.billingStart.IsZero() && !period.billingEnd.IsZero() && period.billingEnd.After(period.billingStart) {
		minutes := int64(period.billingEnd.Sub(period.billingStart) / time.Minute)
		quota.WindowDurationMinutes = &minutes
	}
	return quota
}

func remaining(limit, used *float64) *float64 {
	if limit == nil || used == nil {
		return nil
	}
	value := max(0, *limit-*used)
	return &value
}

func decodeCursorPeriodUsage(data []byte) (cursorPeriodUsage, error) {
	var out cursorPeriodUsage
	for len(data) > 0 {
		number, wireType, n := protowire.ConsumeTag(data)
		if n < 0 {
			return out, protowire.ParseError(n)
		}
		data = data[n:]
		switch number {
		case 1, 2:
			value, consumed := protowire.ConsumeVarint(data)
			if consumed < 0 {
				return out, protowire.ParseError(consumed)
			}
			timestamp := time.UnixMilli(int64(value)).UTC()
			if number == 1 {
				out.billingStart = timestamp
			} else {
				out.billingEnd = timestamp
			}
			data = data[consumed:]
		case 3, 4:
			value, consumed := protowire.ConsumeBytes(data)
			if consumed < 0 {
				return out, protowire.ParseError(consumed)
			}
			var err error
			if number == 3 {
				out.plan, err = decodeCursorPlanUsage(value)
			} else {
				out.spend, err = decodeCursorSpendLimitUsage(value)
			}
			if err != nil {
				return out, err
			}
			data = data[consumed:]
		case 6:
			value, consumed := protowire.ConsumeVarint(data)
			if consumed < 0 {
				return out, protowire.ParseError(consumed)
			}
			out.enabled = value != 0
			data = data[consumed:]
		case 7, 13:
			value, consumed := protowire.ConsumeBytes(data)
			if consumed < 0 {
				return out, protowire.ParseError(consumed)
			}
			if number == 7 {
				out.displayMessage = string(value)
			} else {
				out.autoBucketModels = append(out.autoBucketModels, string(value))
			}
			data = data[consumed:]
		default:
			consumed := protowire.ConsumeFieldValue(number, wireType, data)
			if consumed < 0 {
				return out, protowire.ParseError(consumed)
			}
			data = data[consumed:]
		}
	}
	return out, nil
}

func decodeCursorPlanUsage(data []byte) (*cursorPlanUsage, error) {
	out := &cursorPlanUsage{}
	for len(data) > 0 {
		number, wireType, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		data = data[n:]
		if number >= 1 && number <= 5 || number >= 8 && number <= 11 {
			value, consumed := protowire.ConsumeVarint(data)
			if consumed < 0 {
				return nil, protowire.ParseError(consumed)
			}
			numeric := float64(int32(value))
			switch number {
			case 1:
				out.totalSpend = numeric
			case 2:
				out.includedSpend = numeric
			case 3:
				out.bonusSpend = numeric
			case 4:
				out.remaining = numeric
			case 5:
				out.limit = numeric
			case 8:
				out.autoSpend = &numeric
			case 9:
				out.apiSpend = &numeric
			case 10:
				out.autoLimit = &numeric
			case 11:
				out.apiLimit = &numeric
			}
			data = data[consumed:]
			continue
		}
		if number >= 12 && number <= 14 {
			value, consumed := protowire.ConsumeFixed64(data)
			if consumed < 0 {
				return nil, protowire.ParseError(consumed)
			}
			numeric := math.Float64frombits(value)
			if math.IsNaN(numeric) || math.IsInf(numeric, 0) {
				data = data[consumed:]
				continue
			}
			switch number {
			case 12:
				out.autoPercentUsed = &numeric
			case 13:
				out.apiPercentUsed = &numeric
			case 14:
				out.totalPercentUsed = &numeric
			}
			data = data[consumed:]
			continue
		}
		consumed := protowire.ConsumeFieldValue(number, wireType, data)
		if consumed < 0 {
			return nil, protowire.ParseError(consumed)
		}
		data = data[consumed:]
	}
	return out, nil
}

func decodeCursorSpendLimitUsage(data []byte) (*cursorSpendLimitUsage, error) {
	out := &cursorSpendLimitUsage{}
	for len(data) > 0 {
		number, wireType, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		data = data[n:]
		if number == 8 {
			value, consumed := protowire.ConsumeBytes(data)
			if consumed < 0 {
				return nil, protowire.ParseError(consumed)
			}
			out.limitType = string(value)
			data = data[consumed:]
			continue
		}
		if number >= 1 && number <= 7 || number >= 9 && number <= 11 {
			value, consumed := protowire.ConsumeVarint(data)
			if consumed < 0 {
				return nil, protowire.ParseError(consumed)
			}
			numeric := float64(int32(value))
			switch number {
			case 1:
				out.totalSpend = numeric
			case 2:
				out.pooledLimit = &numeric
			case 3:
				out.pooledUsed = &numeric
			case 4:
				out.pooledRemaining = &numeric
			case 5:
				out.individualLimit = &numeric
			case 6:
				out.individualUsed = numeric
			case 7:
				out.individualRemaining = numeric
			case 9:
				out.overallLimit = &numeric
			case 10:
				out.overallUsed = &numeric
			case 11:
				out.overallRemaining = &numeric
			}
			data = data[consumed:]
			continue
		}
		consumed := protowire.ConsumeFieldValue(number, wireType, data)
		if consumed < 0 {
			return nil, protowire.ParseError(consumed)
		}
		data = data[consumed:]
	}
	return out, nil
}

func parseCursorPlanInfo(data []byte) (*CredentialPlan, error) {
	for len(data) > 0 {
		number, wireType, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		data = data[n:]
		if number == 1 {
			value, consumed := protowire.ConsumeBytes(data)
			if consumed < 0 {
				return nil, protowire.ParseError(consumed)
			}
			return decodeCursorPlanInfo(value)
		}
		consumed := protowire.ConsumeFieldValue(number, wireType, data)
		if consumed < 0 {
			return nil, protowire.ParseError(consumed)
		}
		data = data[consumed:]
	}
	return nil, nil
}

func decodeCursorPlanInfo(data []byte) (*CredentialPlan, error) {
	plan := &CredentialPlan{IncludedUnit: "cents"}
	for len(data) > 0 {
		number, wireType, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		data = data[n:]
		switch number {
		case 1, 3:
			value, consumed := protowire.ConsumeBytes(data)
			if consumed < 0 {
				return nil, protowire.ParseError(consumed)
			}
			if number == 1 {
				plan.Name = string(value)
			} else {
				plan.Price = string(value)
			}
			data = data[consumed:]
		case 2, 4:
			value, consumed := protowire.ConsumeVarint(data)
			if consumed < 0 {
				return nil, protowire.ParseError(consumed)
			}
			if number == 2 {
				amount := float64(int32(value))
				plan.IncludedAmount = &amount
			} else if value > 0 {
				cycleEnd := cursorUnixTime(int64(value))
				plan.BillingCycleEnd = &cycleEnd
			}
			data = data[consumed:]
		default:
			consumed := protowire.ConsumeFieldValue(number, wireType, data)
			if consumed < 0 {
				return nil, protowire.ParseError(consumed)
			}
			data = data[consumed:]
		}
	}
	if plan.Name == "" && plan.Price == "" && plan.IncludedAmount == nil && plan.BillingCycleEnd == nil {
		return nil, nil
	}
	return plan, nil
}

func parseCursorSandUsage(data []byte) (*CredentialQuota, *ResetCredits, error) {
	quota := CredentialQuota{ID: "grok-bot", Name: "Grok Bot"}
	var planLabel string
	var bankedResetCount *int64
	var includedLimitZero, hasAvailableUsage, hasNonZeroIncludedLimit bool
	for len(data) > 0 {
		number, wireType, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil, nil, protowire.ParseError(n)
		}
		data = data[n:]
		switch number {
		case 1, 2:
			value, consumed := protowire.ConsumeBytes(data)
			if consumed < 0 {
				return nil, nil, protowire.ParseError(consumed)
			}
			timestamp, err := decodeProtoTimestamp(value)
			if err != nil {
				return nil, nil, err
			}
			if number == 1 {
				quota.StartsAt = timestamp
			} else {
				quota.ResetsAt = timestamp
			}
			data = data[consumed:]
		case 3:
			value, consumed := protowire.ConsumeFixed64(data)
			if consumed < 0 {
				return nil, nil, protowire.ParseError(consumed)
			}
			percent := math.Float64frombits(value)
			quota.UsedPercent = finiteFloat(&percent)
			data = data[consumed:]
		case 4, 5, 7, 8:
			value, consumed := protowire.ConsumeVarint(data)
			if consumed < 0 {
				return nil, nil, protowire.ParseError(consumed)
			}
			switch number {
			case 4:
				includedLimitZero = value != 0
			case 5:
				count := int64(value)
				bankedResetCount = &count
			case 7:
				hasAvailableUsage = value != 0
			case 8:
				hasNonZeroIncludedLimit = value != 0
			}
			data = data[consumed:]
		case 14, 15:
			value, consumed := protowire.ConsumeBytes(data)
			if consumed < 0 {
				return nil, nil, protowire.ParseError(consumed)
			}
			if number == 15 {
				planLabel = string(value)
			} else if planLabel == "" {
				planLabel = string(value)
			}
			data = data[consumed:]
		default:
			consumed := protowire.ConsumeFieldValue(number, wireType, data)
			if consumed < 0 {
				return nil, nil, protowire.ParseError(consumed)
			}
			data = data[consumed:]
		}
	}
	if quota.StartsAt != nil && quota.ResetsAt != nil && quota.ResetsAt.After(*quota.StartsAt) {
		minutes := int64(quota.ResetsAt.Sub(*quota.StartsAt) / time.Minute)
		quota.WindowDurationMinutes = &minutes
	}
	descriptions := make([]string, 0, 2)
	if planLabel != "" {
		descriptions = append(descriptions, planLabel)
	}
	if includedLimitZero {
		descriptions = append(descriptions, "No included weekly usage")
	} else if hasNonZeroIncludedLimit && !hasAvailableUsage {
		descriptions = append(descriptions, "Included weekly usage exhausted")
	}
	quota.Description = strings.Join(descriptions, " · ")
	var resets *ResetCredits
	if bankedResetCount != nil {
		resets = &ResetCredits{AvailableCount: *bankedResetCount, Credits: make([]ResetCreditDetails, 0)}
	}
	if quota.UsedPercent == nil && quota.StartsAt == nil && quota.ResetsAt == nil && quota.Description == "" {
		return nil, resets, nil
	}
	return &quota, resets, nil
}

func parseCursorHardLimit(data []byte, current *CredentialOnDemand) (*CredentialOnDemand, error) {
	policy, err := decodeCursorHardLimit(data)
	if err != nil {
		return nil, err
	}
	return policy.normalized(current), nil
}

func decodeProtoTimestamp(data []byte) (*time.Time, error) {
	var seconds int64
	var nanos int32
	for len(data) > 0 {
		number, wireType, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		data = data[n:]
		if number == 1 || number == 2 {
			value, consumed := protowire.ConsumeVarint(data)
			if consumed < 0 {
				return nil, protowire.ParseError(consumed)
			}
			if number == 1 {
				seconds = int64(value)
			} else {
				nanos = int32(value)
			}
			data = data[consumed:]
			continue
		}
		consumed := protowire.ConsumeFieldValue(number, wireType, data)
		if consumed < 0 {
			return nil, protowire.ParseError(consumed)
		}
		data = data[consumed:]
	}
	if seconds == 0 && nanos == 0 {
		return nil, nil
	}
	timestamp := time.Unix(seconds, int64(nanos)).UTC()
	return &timestamp, nil
}

func cursorUnixTime(value int64) time.Time {
	if value > 10_000_000_000 {
		return time.UnixMilli(value).UTC()
	}
	return time.Unix(value, 0).UTC()
}

func finiteFloat(value *float64) *float64 {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
		return nil
	}
	return value
}
