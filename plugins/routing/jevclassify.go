package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/plugins/routing/complexity"
)

// ErrSystemOneRequestExecutorNotConfigured means the HTTP server has not
// finished wiring the routing plugin to Bifrost's System One path.
var ErrSystemOneRequestExecutorNotConfigured = errors.New("system one request executor is not configured")

// ErrJevClassificationTimeout reports that a Jev classification exhausted its
// configured budget.
var ErrJevClassificationTimeout = errors.New("jev classification request timed out")

// SystemOneRequestExecutor invokes System One on the bifrost client.
type SystemOneRequestExecutor func(ctx *schemas.BifrostContext, req *schemas.BifrostSystemOneRequest) (*schemas.BifrostSystemOneResponse, *schemas.BifrostError)

// SystemOneExecutorSetter is implemented by routing plugins that accept a
// System One executor, wired by the HTTP server after the bifrost client exists.
type SystemOneExecutorSetter interface {
	SetSystemOneRequestExecutor(SystemOneRequestExecutor)
}

func (p *RoutingPlugin) SetSystemOneRequestExecutor(executor SystemOneRequestExecutor) {
	if executor == nil {
		p.systemOneRequestExecutor.Store(nil)
		if p.jevClassifier != nil {
			p.jevClassifier.SetSystemOneFunc(nil)
		}
		return
	}
	p.systemOneRequestExecutor.Store(&executor)
	if p.jevClassifier != nil {
		p.jevClassifier.SetSystemOneFunc(p.classifyComplexityTextViaJev)
	}
}

func (p *RoutingPlugin) systemOneExecutor() SystemOneRequestExecutor {
	if ptr := p.systemOneRequestExecutor.Load(); ptr != nil {
		return *ptr
	}
	return nil
}

func requestJevClassificationTimeout(jev *complexity.JevConfig) time.Duration {
	if jev != nil && jev.Timeout > 0 {
		return jev.Timeout
	}
	return configstore.DefaultComplexityJevTimeout
}

func (p *RoutingPlugin) classifyComplexityTextViaJev(ctx context.Context, jev *complexity.JevConfig, state string, questions map[string]json.RawMessage) (*schemas.BifrostSystemOneResponse, error) {
	executor := p.systemOneExecutor()
	if executor == nil {
		return nil, ErrSystemOneRequestExecutorNotConfigured
	}
	if jev == nil || jev.Provider == "" || jev.Model == "" {
		return nil, fmt.Errorf("jev classification is not configured")
	}

	timeout := requestJevClassificationTimeout(jev)
	jevCtx := schemas.NewBifrostContext(ctx, time.Now().Add(timeout))
	defer jevCtx.Cancel()
	bifrost.PrepareContextForInternalRequest(jevCtx)

	req := &schemas.BifrostSystemOneRequest{
		Provider:  jev.Provider,
		Model:     jev.Model,
		State:     json.RawMessage(mustJSONString(state)),
		Questions: questions,
	}

	response, bifrostErr := executor(jevCtx, req)
	if bifrostErr != nil {
		if isJevKeyMissing(bifrostErr) {
			return nil, fmt.Errorf("%w: %v", complexity.ErrJevKeyMissing, bifrostErr)
		}
		if isEmbeddingTimeout(jevCtx, bifrostErr) {
			return nil, fmt.Errorf("%w after %s", ErrJevClassificationTimeout, timeout)
		}
		return nil, fmt.Errorf("failed to run jev classification: %v", bifrostErr)
	}
	if response == nil {
		return nil, fmt.Errorf("no response returned from jev classifier provider")
	}

	inputTokens, outputTokens := 0, 0
	if response.Usage != nil {
		if response.Usage.InputTokens != nil && *response.Usage.InputTokens > 0 {
			inputTokens = *response.Usage.InputTokens
		}
		if response.Usage.OutputTokens != nil && *response.Usage.OutputTokens > 0 {
			outputTokens = *response.Usage.OutputTokens
		}
	}
	recordRoutingJevUsage(ctx, jev, inputTokens, outputTokens)
	if p.logger != nil && response.Model != "" {
		p.logger.Info("[Routing] Jev system one model=%s", response.Model)
	}
	return response, nil
}

func mustJSONString(state string) []byte {
	raw, err := json.Marshal(state)
	if err != nil {
		return []byte(`""`)
	}
	return raw
}

func isJevKeyMissing(bifrostErr *schemas.BifrostError) bool {
	if bifrostErr == nil {
		return false
	}
	if bifrostErr.StatusCode != nil && *bifrostErr.StatusCode == 401 {
		return true
	}
	message := strings.ToLower(bifrostErr.GetErrorString())
	switch {
	case strings.Contains(message, "no api key"),
		strings.Contains(message, "no keys"),
		strings.Contains(message, "api key is not configured"),
		strings.Contains(message, "missing or invalid api key"),
		strings.Contains(message, "unauthorized"),
		strings.Contains(message, "authentication failed"):
		return true
	default:
		return false
	}
}

func recordRoutingJevUsage(ctx context.Context, jev *complexity.JevConfig, inputTokens, outputTokens int) {
	bfCtx, ok := ctx.(*schemas.BifrostContext)
	if !ok || jev == nil {
		return
	}
	if inputTokens < 0 {
		inputTokens = 0
	}
	if outputTokens < 0 {
		outputTokens = 0
	}
	provider := string(jev.Provider)
	model := jev.Model
	schemas.AppendRoutingCallOnContext(bfCtx, schemas.BifrostRoutingCall{
		ProviderUsed:       &provider,
		ModelUsed:          &model,
		InputTokens:        &inputTokens,
		OutputTokens:       &outputTokens,
		CountTowardBudgets: jev.CountTowardBudgets,
	})
}

func (p *RoutingPlugin) classifyJevComplexity(ctx *schemas.BifrostContext, input complexity.ComplexityInput) complexityProposal {
	result, err := p.jevClassifier.Classify(ctx, input)
	if err == nil && result != nil {
		out := &complexity.ComplexityResult{Tier: result.Tier}
		confidence := result.Confidence
		message := fmt.Sprintf("Jev complexity: tier=%s confidence=%.3f model=%s", result.Tier, result.Confidence, result.Model)
		if result.Escalated && result.Frontier != nil {
			message = fmt.Sprintf(
				"Jev complexity: tier=%s confidence=%.3f model=%s escalated from MEDIUM (needs_frontier.noul=%.3f)",
				result.Tier, result.Confidence, result.Model, *result.Frontier,
			)
		}
		return complexityProposal{
			Result:     out,
			Mechanism:  complexity.MechanismJev,
			Score:      &confidence,
			LogLevel:   schemas.LogLevelInfo,
			LogMessage: message,
		}
	}

	if err != nil && p.logger != nil {
		p.logger.Debug("[Routing] Jev complexity classification unavailable: %v", err)
	}
	unavailableLog := "Jev complexity classification unavailable, so no complexity tier is published"
	failClosed := false
	switch {
	case errors.Is(err, complexity.ErrJevKeyMissing):
		unavailableLog = "Jev classifier selected but no API key is configured; failing closed"
		failClosed = true
	case errors.Is(err, ErrJevClassificationTimeout):
		unavailableLog = fmt.Sprintf(
			"Jev complexity classification timed out after %s, so no complexity tier is published",
			p.jevClassifier.Timeout(),
		)
	case errors.Is(err, complexity.ErrJevLowConfidence):
		unavailableLog = fmt.Sprintf("Jev complexity confidence below threshold, so no complexity tier is published: %v", err)
	case err != nil:
		unavailableLog = fmt.Sprintf("Jev complexity classification unavailable: %v; no complexity tier is published", err)
	}
	return complexityProposal{
		Mechanism:  complexity.MechanismSkipped,
		LogLevel:   schemas.LogLevelWarn,
		LogMessage: unavailableLog,
		FailClosed: failClosedError(failClosed, err),
	}
}

func failClosedError(failClosed bool, err error) error {
	if !failClosed {
		return nil
	}
	if err != nil {
		return err
	}
	return complexity.ErrJevKeyMissing
}
