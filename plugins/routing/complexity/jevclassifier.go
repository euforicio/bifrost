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
	jevQuestionComplexityTier  = "complexity_tier"
	jevQuestionNeedsFrontier   = "needs_frontier"
	jevQuestionComplexityScore = "complexity_score"

	jevChoiceOther   = "OTHER"
	jevChoiceUnknown = "UNKNOWN"
)

// ErrJevKeyMissing reports that Jev was selected but the configured provider
// has no usable API key. Model routing fails open (no tier); this error is
// for logs. Future safety/tool gates may fail closed on it.
var ErrJevKeyMissing = errors.New("jev classifier selected but no api key is configured")

// ErrJevLowConfidence reports that Choice.confidence was below min_confidence.
// The classifier publishes no tier (same degrade as a semantic miss).
var ErrJevLowConfidence = errors.New("jev choice confidence below min_confidence")

// ErrJevUnresolvedChoice reports that Jev chose OTHER or UNKNOWN so the
// request does not force SIMPLE/MEDIUM/COMPLEX.
var ErrJevUnresolvedChoice = errors.New("jev choice is OTHER or UNKNOWN")

type jevEvalOnceKey struct{}

type jevEvalOnceState struct {
	once   sync.Once
	result *JevResult
	err    error
}

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
	Tier          string
	Confidence    float64
	Probabilities map[string]float64
	Model         string
	Frontier      *float64
	Score         *float64
	Escalated     bool
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
	if state := jevEvalOnceFromContext(ctx); state != nil {
		state.once.Do(func() {
			state.result, state.err = c.classifyUncached(ctx, input)
		})
		return state.result, state.err
	}
	return c.classifyUncached(ctx, input)
}

func (c *JevClassifier) classifyUncached(ctx context.Context, input ComplexityInput) (*JevResult, error) {
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

// jevComplexityQuestions is one System One call: contrastive Choice
// SIMPLE|MEDIUM|COMPLEX plus OTHER/UNKNOWN so weak matches do not force a
// tier, optional Noul, and optional Score. Compose happens in code.
func jevComplexityQuestions() (map[string]json.RawMessage, error) {
	choice := map[string]any{
		"type": "choice",
		"instructions": map[string]any{
			"question": "Which complexity tier should handle this user request?",
			"focus":    "Judge required model capability, not topic or verbosity. Prefer OTHER or UNKNOWN when no tier is a clear fit.",
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
			jevChoiceOther: map[string]any{
				"what":     "The request does not match SIMPLE, MEDIUM, or COMPLEX",
				"not_for":  "Any request that clearly fits one of those three tiers",
				"examples": []string{"unrelated attachment with no user ask"},
			},
			jevChoiceUnknown: map[string]any{
				"what":     "Too little signal to judge required model capability",
				"not_for":  "A request whose required capability is readable from the latest user message",
				"examples": []string{"...", "ok", "this"},
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
	score := map[string]any{
		"type":         "score",
		"instructions": "Rate required model capability from 1 (trivial) to 3 (frontier reasoning). Do not count tokens, dates, or steps.",
		"criteria": map[string]any{
			"1": "SIMPLE: trivial lookup, greeting, short rewrite, single-step fact",
			"2": "MEDIUM: bounded implementation or analysis with clear scope",
			"3": "COMPLEX: ambiguous root-cause, architecture tradeoffs, multi-system reasoning",
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
	scoreRaw, err := json.Marshal(score)
	if err != nil {
		return nil, err
	}
	return map[string]json.RawMessage{
		jevQuestionComplexityTier:  choiceRaw,
		jevQuestionNeedsFrontier:   noulRaw,
		jevQuestionComplexityScore: scoreRaw,
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

type jevScoreAnswer struct {
	Type          string             `json:"type"`
	Score         *float64           `json:"score"`
	Confidence    *float64           `json:"confidence"`
	Legend        map[string]string  `json:"legend"`
	Probabilities map[string]float64 `json:"probabilities"`
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
	if isJevUnresolvedChoice(tier) {
		return nil, fmt.Errorf("%w: %s", ErrJevUnresolvedChoice, tier)
	}
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
		Tier:          tier,
		Confidence:    confidence,
		Probabilities: choice.Probabilities,
		Model:         response.Model,
	}
	if rawNoul, ok := response.Answers[jevQuestionNeedsFrontier]; ok && len(rawNoul) > 0 {
		var noul jevNoulAnswer
		if err := json.Unmarshal(rawNoul, &noul); err == nil && noul.Noul != nil {
			result.Frontier = noul.Noul
		}
	}
	var scoreConfidence float64
	if rawScore, ok := response.Answers[jevQuestionComplexityScore]; ok && len(rawScore) > 0 {
		var score jevScoreAnswer
		if err := json.Unmarshal(rawScore, &score); err == nil && score.Score != nil {
			result.Score = score.Score
			if score.Confidence != nil {
				scoreConfidence = *score.Confidence
			}
		}
	}
	// Compose in code against the original Choice. Score never names a
	// discrete tier on its own. Do not treat noul as a Choice probability.
	if result.Tier == TierMedium && confidence < 0.8 {
		if result.Frontier != nil && *result.Frontier >= configstore.DefaultComplexityJevFrontierNoul {
			result.Tier = TierComplex
			result.Escalated = true
		} else if result.Score != nil && *result.Score >= 2.5 && scoreConfidence >= minConfidence {
			result.Tier = TierComplex
			result.Escalated = true
		}
	}
	return result, nil
}

func isJevUnresolvedChoice(tier string) bool {
	return tier == jevChoiceOther || tier == jevChoiceUnknown
}

func jevEvalOnceFromContext(ctx context.Context) *jevEvalOnceState {
	bfCtx, ok := ctx.(*schemas.BifrostContext)
	if !ok || bfCtx == nil {
		return nil
	}
	if existing, ok := bfCtx.Value(jevEvalOnceKey{}).(*jevEvalOnceState); ok && existing != nil {
		return existing
	}
	state := &jevEvalOnceState{}
	bfCtx.SetValue(jevEvalOnceKey{}, state)
	return state
}
