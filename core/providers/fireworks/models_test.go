package fireworks

import (
	"net/url"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

func TestModelCatalogURLUsesManagementAPIAndServerlessFilter(t *testing.T) {
	provider := &FireworksProvider{networkConfig: schemas.NetworkConfig{BaseURL: "https://api.fireworks.ai/inference"}}
	parsed, err := url.Parse(provider.modelCatalogURL("next page"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/v1/accounts/fireworks/models" {
		t.Fatalf("unexpected catalog path: %s", parsed.Path)
	}
	if parsed.Query().Get("filter") != "supports_serverless=true" || parsed.Query().Get("pageSize") != "200" || parsed.Query().Get("pageToken") != "next page" {
		t.Fatalf("unexpected catalog query: %s", parsed.RawQuery)
	}
}

func TestFireworksSupportedMethodsReflectModelKind(t *testing.T) {
	embedding := fireworksSupportedMethods(fireworksModel{Name: "accounts/fireworks/models/qwen3-embedding-8b", Kind: "EMBEDDING_MODEL"})
	if len(embedding) != 1 || embedding[0] != string(schemas.EmbeddingRequest) {
		t.Fatalf("unexpected embedding methods: %#v", embedding)
	}
	chat := fireworksSupportedMethods(fireworksModel{Name: "accounts/fireworks/models/kimi-k3", SupportsTools: true})
	if len(chat) != 6 || chat[2] != string(schemas.ChatCompletionRequest) || chat[4] != string(schemas.ResponsesRequest) {
		t.Fatalf("unexpected chat methods: %#v", chat)
	}
}
