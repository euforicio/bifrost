package schemas

import "encoding/json"

// BifrostSystemOneRequest is the native TypeSafe System One evaluate request.
// It is not a chat completion: Jev returns typed answers, not generated text.
type BifrostSystemOneRequest struct {
	Provider ModelProvider `json:"provider"`
	Model    string        `json:"model"`
	// State is the content to evaluate. Official TypeSafe docs accept a string,
	// object, array, or null. Callers should send only the latest user message
	// (or a small structured extract), not full conversation history.
	State json.RawMessage `json:"state"`
	// Questions is a nonempty map of typed questions keyed by caller-chosen ids.
	// Answers come back under the same keys. Each value is a documented Question
	// (noul | choice | score) — see https://docs.typesafe.ai/api
	Questions map[string]json.RawMessage `json:"questions"`
	Fallbacks      []Fallback `json:"fallbacks,omitempty"`
	RawRequestBody []byte     `json:"-"`
}

// GetRawRequestBody returns the raw request body for the System One request.
func (r *BifrostSystemOneRequest) GetRawRequestBody() []byte {
	if r == nil {
		return nil
	}
	return r.RawRequestBody
}

// SystemOneUsage is the documented token usage object on a System One response.
// Output tokens are free per TypeSafe's public pricing; both fields are optional
// because the API may omit them.
type SystemOneUsage struct {
	InputTokens  *int `json:"input_tokens,omitempty"`
	OutputTokens *int `json:"output_tokens,omitempty"`
}

// BifrostSystemOneResponse is the native TypeSafe System One evaluate response.
type BifrostSystemOneResponse struct {
	// Model is the versioned ID that answered (aliases such as jev-latest resolve
	// to jev-1.13.0). Log this; do not invent a similarity score from it.
	Model       string                     `json:"model"`
	Answers     map[string]json.RawMessage `json:"answers"`
	Usage       *SystemOneUsage            `json:"usage,omitempty"`
	ExtraFields BifrostResponseExtraFields `json:"extra_fields"`
}
