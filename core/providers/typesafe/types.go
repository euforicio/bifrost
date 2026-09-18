package typesafe

import "encoding/json"

// # SYSTEM ONE TYPES
// Field names match https://docs.typesafe.ai/api exactly. Do not invent extras.

// TypeSafeSystemOneRequest is the POST /v1/systemone body.
type TypeSafeSystemOneRequest struct {
	State     json.RawMessage                    `json:"state"`
	Model     string                             `json:"model,omitempty"`
	Questions map[string]TypeSafeSystemOneQuestion `json:"questions"`
}

// GetExtraParams satisfies providerUtils.RequestBodyWithExtraParams. System One
// has no extra-params bag in the documented API.
func (r *TypeSafeSystemOneRequest) GetExtraParams() map[string]interface{} {
	return nil
}

// TypeSafeSystemOneQuestion is one noul | choice | score question.
// instructions and criteria accept string, object, array, or null (EntryType).
type TypeSafeSystemOneQuestion struct {
	Type         string          `json:"type"`
	Instructions json.RawMessage `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
}

// TypeSafeSystemOneResponse is the POST /v1/systemone success body.
type TypeSafeSystemOneResponse struct {
	Model   string                         `json:"model"`
	Answers map[string]TypeSafeSystemOneAnswer `json:"answers"`
	Usage   *TypeSafeSystemOneUsage        `json:"usage,omitempty"`
}

// TypeSafeSystemOneUsage is the documented usage object.
type TypeSafeSystemOneUsage struct {
	InputTokens  *int `json:"input_tokens,omitempty"`
	OutputTokens *int `json:"output_tokens,omitempty"`
}

// TypeSafeSystemOneAnswer is a discriminated answer. Only documented fields are
// present: noul answers have noul (no confidence); choice answers have choice,
// probabilities, and confidence; score answers have score, legend, probabilities,
// and confidence.
type TypeSafeSystemOneAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        *string            `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
}

// TypeSafeErrorBody is the JSON error envelope TypeSafe returns with 401/422/429/529.
// The official docs only guarantee a JSON body describing what went wrong; we
// accept a message field when present and otherwise surface the raw body.
type TypeSafeErrorBody struct {
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// # MODELS TYPES

// TypeSafeListModelsResponse is the GET /v1/models body.
// Official shape: { "models": [ { "name", "description", "release_date" } ] }
type TypeSafeListModelsResponse struct {
	Models []TypeSafeModelCard `json:"models"`
}

// TypeSafeModelCard is one model or alias listed by GET /v1/models.
type TypeSafeModelCard struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ReleaseDate string `json:"release_date"`
}
