package complexity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
)

const (
	jevQuestionComplexityTier = "complexity_tier"
	jevQuestionNeedsFrontier  = "needs_frontier"
)

// ErrJevKeyMissing reports that Jev was selected as the classifier but the
// configured provider has no usable API key. Callers must fail closed.
var ErrJevKeyMissing = errors.New("jev classifier selected but no api key is configured")

// ErrJevLowConfidence reports that Choice.confidence was below min_confidence.
// The classifier publishes no tier (same degrade as a semantic miss).
var ErrJevLowConfidence = errors.New("jev choice confidence below min_confidence")

// JevStatus is the coarse readiness of Jev classification.
type JevStatus string

const (
	JevStatusDisabled JevStatus = "disabled"
	JevStatusReady    JevStatus = "ready"
)

// JevStatusInfo is the safe runtime state exposed to handlers and UI clients.
type JevStatusInfo struct {
	State JevStatus `json:"state"`
}

// JevResult is the composed classification. Confidence is the documented
// Choice.confidence — never an invented similarity score.
type JevResult struct {
	Tier       string
	Confidence float64
	Model      string
	Frontier   *float64
	Escalated  bool
}

// SystemOneFunc executes one System One request through the configured
// provider+model (Bifrost key selection, retries, network config).
type SystemOneFunc func(ctx context.Context, jev *JevConfig, state string, questions map[string]json.RawMessage) (*schemas.BifrostSystemOneResponse, error)

// JevClassifier classifies a request with one TypeSafe System One call:
// a contrastive Choice SIMPLE|MEDIUM|COMPLEX plus an optional Noul that
// code composes afterwards.
type JevClassifier struct {
	logger schemas.Logger

	mu        sync.Mutex
	config    *AnalyzerConfig
	systemOne SystemOneFunc
}

func NewJevClassifier(logger schemas.Logger) *JevClassifier {
	return &JevClassifier{logger: logger}
}

func (c *JevClassifier) Configure(config *AnalyzerConfig) {
	c.mu.Lock()
	c.config = cloneAnalyzerConfig(config)
	c.mu.Unlock()
}

func (c *JevClassifier) SetSystemOneFunc(fn SystemOneFunc) {
	c.mu.Lock()
	c.systemOne = fn
	c.mu.Unlock()
}

func (c *JevClassifier) IsConfigured() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.config != nil && c.config.Jev != nil
}

func (c *JevClassifier) FallbackEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.config.JevFallbackEnabled()
}

func (c *JevClassifier) PrimaryEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.config.JevPrimaryEnabled()
}

func (c *JevClassifier) Status() JevStatusInfo {
	if c.IsConfigured() {
		return JevStatusInfo{State: JevStatusReady}
	}
	return JevStatusInfo{State: JevStatusDisabled}
}

func (c *JevClassifier) Timeout() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.config == nil || c.config.Jev == nil || c.config.Jev.Timeout <= 0 {
		return configstore.DefaultComplexityJevTimeout
	}
	return c.config.Jev.Timeout
}

func (c *JevClassifier) Classify(ctx context.Context, input ComplexityInput) (*JevResult, error) {
	c.mu.Lock()
	if c.config == nil || c.config.Jev == nil || c.systemOne == nil {
		c.mu.Unlock()
		return nil, nil
	}
	jev := cloneJevConfig(c.config.Jev)
	systemOne := c.systemOne
	c.mu.Unlock()

	// Jaggedness: send only the latest user message. History stays in code.
	state := truncateJevState(input.LastUserText)
	if state == "" {
		return nil, nil
	}

	questions, err := jevComplexityQuestions()
	if err != nil {
		return nil, err
	}
	response, err := systemOne(ctx, jev, state, questions)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, fmt.Errorf("jev classifier returned no response")
	}
	return composeJevResult(response, jev.MinConfidence)
}

// maxJevStateChars keeps state well under TypeSafe's 32k state+question
// budget. Truncation is done in code — Jev must not do length math.
const maxJevStateChars = 24000

func truncateJevState(state string) string {
	state = strings.TrimSpace(state)
	if len(state) <= maxJevStateChars {
		return state
	}
	return state[:maxJevStateChars]
}

func cloneJevConfig(jev *JevConfig) *JevConfig {
	if jev == nil {
		return nil
	}
	clone := *jev
	return &clone
}

// jevComplexityQuestions is the documented contrastive Choice plus optional
// parallel Noul. Criteria are structured what/not_for/examples — no math or
// dates, no vague "how complex is this?" Score.
func jevComplexityQuestions() (map[string]json.RawMessage, error) {
	choice := map[string]any{
		"type": "choice",
		"instructions": map[string]any{
			"question": "Which complexity tier should handle this user request?",
			"focus":    "Judge required model capability, not topic or verbosity.",
		},
		"criteria": map[string]any{
			"SIMPLE": map[string]any{
				"what":     "Trivial lookup, greeting, short rewrite, single-step fact",
				"not_for":  "Multi-step debugging, architecture, or tool-heavy agent work",
				"examples": []string{"what is a mutex?", "fix the grammar in this sentence"},
			},
			"MEDIUM": map[string]any{
				"what":     "Bounded implementation or analysis with clear scope",
				"not_for":  "Greenfield design or deep multi-file investigation",
				"examples": []string{"add api-key auth: hash keys, reject revoked, never log them"},
			},
			"COMPLEX": map[string]any{
				"what":     "Ambiguous root-cause, architecture tradeoffs, multi-system reasoning",
				"not_for":  "Simple Q&A or single-line edits",
				"examples": []string{"balance testing rules and staffing against rising resistant infections"},
			},
		},
	}
	noul := map[string]any{
		"type":         "noul",
		"instructions": "Does this request require a frontier-class reasoning model rather than a small/fast model?",
		"criteria": map[string]any{
			"true":  "Needs deep multi-hop reasoning, novel design, or hard debugging",
			"false": "A small/fast model can handle it safely",
		},
	}
	choiceRaw, err := json.Marshal(choice)
	if err != nil {
		return nil, err
	}
	noulRaw, err := json.Marshal(noul)
	if err != nil {
		return nil, err
	}
	return map[string]json.RawMessage{
		jevQuestionComplexityTier: choiceRaw,
		jevQuestionNeedsFrontier:  noulRaw,
	}, nil
}

type jevChoiceAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Confidence    *float64           `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

type jevNoulAnswer struct {
	Type string   `json:"type"`
	Noul *float64 `json:"noul"`
}

func composeJevResult(response *schemas.BifrostSystemOneResponse, minConfidence float64) (*JevResult, error) {
	if minConfidence <= 0 {
		minConfidence = configstore.DefaultComplexityJevMinConfidence
	}
	rawChoice, ok := response.Answers[jevQuestionComplexityTier]
	if !ok || len(rawChoice) == 0 {
		return nil, fmt.Errorf("jev response missing %s answer", jevQuestionComplexityTier)
	}
	var choice jevChoiceAnswer
	if err := json.Unmarshal(rawChoice, &choice); err != nil {
		return nil, fmt.Errorf("failed to parse jev choice answer: %w", err)
	}
	tier := strings.ToUpper(strings.TrimSpace(choice.Choice))
	if !isComplexityTier(tier) {
		return nil, fmt.Errorf("jev choice named no complexity tier: %q", choice.Choice)
	}
	confidence := 0.0
	if choice.Confidence != nil {
		confidence = *choice.Confidence
	}
	if confidence < minConfidence {
		return nil, fmt.Errorf("%w: %.3f < %.3f", ErrJevLowConfidence, confidence, minConfidence)
	}

	result := &JevResult{
		Tier:       tier,
		Confidence: confidence,
		Model:      response.Model,
	}
	if rawNoul, ok := response.Answers[jevQuestionNeedsFrontier]; ok && len(rawNoul) > 0 {
		var noul jevNoulAnswer
		if err := json.Unmarshal(rawNoul, &noul); err == nil && noul.Noul != nil {
			result.Frontier = noul.Noul
			// Compose in code: MEDIUM + high frontier noul + middling Choice
			// confidence escalates to COMPLEX. Do not treat noul as Choice
			// probability identity.
			if result.Tier == TierMedium &&
				*noul.Noul >= configstore.DefaultComplexityJevFrontierNoul &&
				confidence < 0.8 {
				result.Tier = TierComplex
				result.Escalated = true
			}
		}
	}
	return result, nil
}
