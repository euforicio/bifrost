package providercredentials

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/schemas"
	"google.golang.org/protobuf/encoding/protowire"
)

const (
	cursorSetHardLimitPath          = "/aiserver.v1.DashboardService/SetHardLimit"
	maxCursorHardLimitDollars int32 = 1<<31 - 1
)

var (
	ErrOnDemandConflict            = errors.New("provider on-demand settings changed")
	ErrOnDemandManagedByOrg        = errors.New("provider on-demand settings are managed by the organization")
	ErrOnDemandUnsupportedProvider = errors.New("provider does not support on-demand updates")
)

// UpdateCredentialOnDemandRequest updates Cursor's personal on-demand policy.
// Cursor's DashboardService expresses limits in whole dollars.
type UpdateCredentialOnDemandRequest struct {
	Enabled              bool   `json:"enabled"`
	LimitDollars         int32  `json:"limit_dollars"`
	ExpectedEnabled      *bool  `json:"expected_enabled,omitempty"`
	ExpectedLimitDollars *int32 `json:"expected_limit_dollars,omitempty"`
}

type cursorHardLimitPolicy struct {
	hardLimitDollars                               int32
	noUsageBasedAllowed                            bool
	hardLimitPerUser                               *int32
	perUserMonthlyLimitDollars                     int32
	isDynamicTeamLimit                             bool
	disabledByOrganization                         bool
	perUserFirstPartyModelsAdditionalBudgetDollars *int32
}

type cursorSetHardLimitRequest struct {
	hardLimitDollars                               int32
	noUsageBasedAllowed                            bool
	preserveHardLimitPerUser                       bool
	perUserMonthlyLimitDollars                     int32
	isDynamicTeamLimit                             bool
	perUserFirstPartyModelsAdditionalBudgetDollars *int32
}

func (m *Manager) UpdateOnDemand(
	ctx context.Context,
	provider schemas.ModelProvider,
	credentialID string,
	update UpdateCredentialOnDemandRequest,
) (*CredentialOnDemand, error) {
	if provider != ProviderCursor {
		return nil, ErrOnDemandUnsupportedProvider
	}
	if update.LimitDollars < 0 || (update.Enabled && update.LimitDollars == 0) {
		return nil, errors.New("on-demand limit must be a positive whole-dollar amount when enabled")
	}

	credential, err := m.resolveProviderCredentialForUsage(ctx, provider, credentialID, false)
	if err != nil {
		return nil, err
	}
	request := func(path string, body []byte) (usageHTTPResponse, error) {
		requestCtx, cancel := context.WithTimeout(ctx, providerUsageTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, m.endpoints.cursorAPIBase+path, bytes.NewReader(body))
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
	forcedRefresh := false
	call := func(path string, body []byte) (usageHTTPResponse, error) {
		resp, err := request(path, body)
		if err != nil || (resp.status != http.StatusUnauthorized && resp.status != http.StatusForbidden) || forcedRefresh {
			return resp, err
		}
		forcedRefresh = true
		credential, err = m.resolveProviderCredentialForUsage(ctx, provider, credentialID, true)
		if err != nil {
			return usageHTTPResponse{}, err
		}
		return request(path, body)
	}

	currentResponse, err := call(cursorHardLimitPath, nil)
	if err != nil {
		return nil, err
	}
	if currentResponse.status < http.StatusOK || currentResponse.status >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("cursor hard limit returned status %d", currentResponse.status)
	}
	current, err := decodeCursorHardLimit(currentResponse.body)
	if err != nil {
		return nil, err
	}
	if current.isManagedPolicy() {
		return nil, ErrOnDemandManagedByOrg
	}
	periodResponse, err := call(cursorCurrentPeriodUsagePath, nil)
	if err != nil {
		return nil, err
	}
	if periodResponse.status < http.StatusOK || periodResponse.status >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("cursor current-period usage returned status %d", periodResponse.status)
	}
	period, err := decodeCursorPeriodUsage(periodResponse.body)
	if err != nil {
		return nil, err
	}
	if period.spend == nil || !isIndividualLimitType(period.spend.limitType) {
		return nil, ErrOnDemandManagedByOrg
	}
	currentEnabled := !current.noUsageBasedAllowed
	currentLimit := current.effectiveLimitDollars()
	if update.ExpectedEnabled != nil && *update.ExpectedEnabled != currentEnabled {
		return nil, ErrOnDemandConflict
	}
	if update.ExpectedLimitDollars != nil && *update.ExpectedLimitDollars != currentLimit {
		return nil, ErrOnDemandConflict
	}

	payload := encodeCursorSetHardLimitRequest(cursorSetHardLimitRequest{
		hardLimitDollars:                               update.LimitDollars,
		noUsageBasedAllowed:                            !update.Enabled,
		preserveHardLimitPerUser:                       true,
		perUserMonthlyLimitDollars:                     current.perUserMonthlyLimitDollars,
		isDynamicTeamLimit:                             current.isDynamicTeamLimit,
		perUserFirstPartyModelsAdditionalBudgetDollars: current.perUserFirstPartyModelsAdditionalBudgetDollars,
	})
	setResponse, err := call(cursorSetHardLimitPath, payload)
	if err != nil {
		return nil, err
	}
	if setResponse.status < http.StatusOK || setResponse.status >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("cursor hard limit update returned status %d", setResponse.status)
	}

	verifiedResponse, err := call(cursorHardLimitPath, nil)
	if err != nil {
		return nil, err
	}
	if verifiedResponse.status < http.StatusOK || verifiedResponse.status >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("cursor hard limit verification returned status %d", verifiedResponse.status)
	}
	verified, err := decodeCursorHardLimit(verifiedResponse.body)
	if err != nil {
		return nil, err
	}
	if verified.disabledByOrganization || (!verified.noUsageBasedAllowed) != update.Enabled || verified.effectiveLimitDollars() != update.LimitDollars {
		return nil, errors.New("cursor did not apply the requested on-demand settings")
	}
	return verified.normalized(&CredentialOnDemand{LimitType: period.spend.limitType}), nil
}

func (policy cursorHardLimitPolicy) effectiveLimitDollars() int32 {
	if policy.hardLimitDollars > 0 {
		return policy.hardLimitDollars
	}
	return policy.perUserMonthlyLimitDollars
}

func (policy cursorHardLimitPolicy) isManagedPolicy() bool {
	return policy.disabledByOrganization || policy.isDynamicTeamLimit || policy.hardLimitPerUser != nil
}

func isIndividualLimitType(limitType string) bool {
	switch strings.ToLower(strings.TrimSpace(limitType)) {
	case "individual", "user":
		return true
	default:
		return false
	}
}

func (policy cursorHardLimitPolicy) normalized(current *CredentialOnDemand) *CredentialOnDemand {
	out := &CredentialOnDemand{Enabled: true, Unit: "cents"}
	if current != nil {
		*out = *current
	}
	out.Enabled = !policy.noUsageBasedAllowed && !policy.disabledByOrganization
	out.CanUpdate = !policy.isManagedPolicy() && current != nil && isIndividualLimitType(current.LimitType)
	limitDollars := policy.effectiveLimitDollars()
	if limitDollars > 0 && limitDollars < maxCursorHardLimitDollars {
		limit := float64(limitDollars) * 100
		out.Limit = &limit
		out.Remaining = remaining(out.Limit, out.Used)
	}
	if policy.isManagedPolicy() {
		out.DisabledReason = "Disabled by organization policy"
	} else if policy.noUsageBasedAllowed {
		out.DisabledReason = "On-demand spending is disabled"
	} else {
		out.DisabledReason = ""
	}
	return out
}

func decodeCursorHardLimit(data []byte) (cursorHardLimitPolicy, error) {
	var out cursorHardLimitPolicy
	for len(data) > 0 {
		number, wireType, n := protowire.ConsumeTag(data)
		if n < 0 {
			return out, protowire.ParseError(n)
		}
		data = data[n:]
		if wireType != protowire.VarintType {
			consumed := protowire.ConsumeFieldValue(number, wireType, data)
			if consumed < 0 {
				return out, protowire.ParseError(consumed)
			}
			data = data[consumed:]
			continue
		}
		value, consumed := protowire.ConsumeVarint(data)
		if consumed < 0 {
			return out, protowire.ParseError(consumed)
		}
		numeric := int32(value)
		switch number {
		case 1:
			out.hardLimitDollars = numeric
		case 2:
			out.noUsageBasedAllowed = value != 0
		case 3:
			out.hardLimitPerUser = &numeric
		case 4:
			out.perUserMonthlyLimitDollars = numeric
		case 5:
			out.isDynamicTeamLimit = value != 0
		case 10:
			out.disabledByOrganization = value != 0
		case 11:
			out.perUserFirstPartyModelsAdditionalBudgetDollars = &numeric
		}
		data = data[consumed:]
	}
	return out, nil
}

func encodeCursorSetHardLimitRequest(request cursorSetHardLimitRequest) []byte {
	payload := appendProtoInt32(nil, 2, request.hardLimitDollars)
	payload = appendProtoBool(payload, 3, request.noUsageBasedAllowed)
	payload = appendProtoBool(payload, 5, request.preserveHardLimitPerUser)
	if request.perUserMonthlyLimitDollars > 0 {
		payload = appendProtoInt32(payload, 6, request.perUserMonthlyLimitDollars)
	}
	if request.isDynamicTeamLimit {
		payload = appendProtoBool(payload, 8, true)
	}
	if request.perUserFirstPartyModelsAdditionalBudgetDollars != nil {
		payload = appendProtoInt32(payload, 11, *request.perUserFirstPartyModelsAdditionalBudgetDollars)
	}
	return payload
}

func decodeCursorSetHardLimitRequest(data []byte) (cursorSetHardLimitRequest, error) {
	var out cursorSetHardLimitRequest
	for len(data) > 0 {
		number, wireType, n := protowire.ConsumeTag(data)
		if n < 0 {
			return out, protowire.ParseError(n)
		}
		data = data[n:]
		value, consumed := protowire.ConsumeVarint(data)
		if consumed < 0 {
			return out, protowire.ParseError(consumed)
		}
		switch number {
		case 2:
			out.hardLimitDollars = int32(value)
		case 3:
			out.noUsageBasedAllowed = value != 0
		case 5:
			out.preserveHardLimitPerUser = value != 0
		case 6:
			out.perUserMonthlyLimitDollars = int32(value)
		case 8:
			out.isDynamicTeamLimit = value != 0
		case 11:
			numeric := int32(value)
			out.perUserFirstPartyModelsAdditionalBudgetDollars = &numeric
		default:
			if wireType != protowire.VarintType {
				return out, errors.New("unexpected Cursor hard-limit request wire type")
			}
		}
		data = data[consumed:]
	}
	return out, nil
}

func appendProtoInt32(dst []byte, number protowire.Number, value int32) []byte {
	dst = protowire.AppendTag(dst, number, protowire.VarintType)
	return protowire.AppendVarint(dst, uint64(uint32(value)))
}

func appendProtoBool(dst []byte, number protowire.Number, value bool) []byte {
	dst = protowire.AppendTag(dst, number, protowire.VarintType)
	if value {
		return protowire.AppendVarint(dst, 1)
	}
	return protowire.AppendVarint(dst, 0)
}
