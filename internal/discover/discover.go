package discover

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
)

// httpClient is shared across all discovery and enrichment calls. It
// has a reasonable timeout so individual requests cannot block forever
// even if the caller forgets to set a context deadline.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// stripV1Suffix removes a trailing /v1 from a base URL. Enricher
// endpoints (e.g. Ollama's /api/show, LM Studio's /api/v1/models) are
// served at the server root, not under the OpenAI-compatible /v1
// prefix. Since provider configs typically include /v1 in the base URL
// for chat completions, enrichers must strip it before constructing
// their own request paths.
func stripV1Suffix(baseURL string) string {
	return strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
}

// doRequest builds and executes an authenticated HTTP request using the
// shared client. It resolves variable references in the base URL, API
// key, and extra headers via the provided Resolver. The path is joined
// to the base URL with proper slash handling.
func doRequest(ctx context.Context, method, baseURL, path, apiKey string, extraHeaders map[string]string, resolver Resolver, body any) (*http.Response, error) {
	resolvedBase, _ := resolver.ResolveValue(baseURL)
	resolvedKey, _ := resolver.ResolveValue(apiKey)

	url := strings.TrimRight(resolvedBase, "/") + "/" + strings.TrimLeft(path, "/")

	var reqBody *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshaling request body: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	var req *http.Request
	var err error
	if reqBody != nil {
		req, err = http.NewRequestWithContext(ctx, method, url, reqBody)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, url, nil)
	}
	if err != nil {
		return nil, err
	}

	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if resolvedKey != "" {
		req.Header.Set("Authorization", "Bearer "+resolvedKey)
	}
	for k, v := range extraHeaders {
		resolved, err := resolver.ResolveValue(v)
		if err != nil || resolved == "" {
			continue
		}
		req.Header.Set(k, resolved)
	}

	return httpClient.Do(req)
}

// Config holds the provider configuration needed for model discovery.
type Config struct {
	ID           string
	BaseURL      string
	APIKey       string
	ExtraHeaders map[string]string
	// Existing models from config — IDs present in this list are skipped
	// during discovery (user-specified models win).
	ExistingModels []catwalk.Model
}

// Resolver resolves variable references (e.g. $ENV_VAR) in config values.
type Resolver interface {
	ResolveValue(val string) (string, error)
}

type modelsResponse struct {
	Data []modelEntry `json:"data"`
}

// modelEntry is one /v1/models entry. The OpenAI shape carries only id,
// object, created and owned_by; serving engines and gateways add the
// metadata a client needs to size a request, under names that differ by
// engine. Every spelling seen in the wild is read here so a gateway that
// fronts SGLang, vLLM, llama.cpp or a router yields usable models
// without an engine-specific enricher.
type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	// Endpoint names the API a non-chat model serves (vLLM-Omni: "/v1/audio/speech").
	Endpoint string `json:"endpoint"`
	// Context window, by the names vLLM/SGLang, LiteLLM, Ollama-style
	// proxies and OpenRouter use.
	MaxModelLen   int64 `json:"max_model_len"`
	ContextLength int64 `json:"context_length"`
	ContextWindow int64 `json:"context_window"`
	MaxInputToken int64 `json:"max_input_tokens"`
	// Output ceiling, by the names LiteLLM and OpenRouter use.
	MaxOutputTokens int64 `json:"max_output_tokens"`
	MaxTokens       int64 `json:"max_tokens"`
	// Name is a display name some gateways attach.
	Name string `json:"name"`
}

// contextWindow returns the entry's context window under whichever name
// it was published, or 0.
func (e modelEntry) contextWindow() int64 {
	for _, v := range []int64{e.MaxModelLen, e.ContextLength, e.ContextWindow, e.MaxInputToken} {
		if v > 0 {
			return v
		}
	}
	return 0
}

// maxOutput returns the entry's output ceiling, or 0.
func (e modelEntry) maxOutput() int64 {
	for _, v := range []int64{e.MaxOutputTokens, e.MaxTokens} {
		if v > 0 {
			return v
		}
	}
	return 0
}

// servesChat reports whether the entry is a chat model. Gateways that also
// host speech or embedding models label them with the endpoint they
// answer; those must not appear in a model picker.
func (e modelEntry) servesChat() bool {
	switch e.Endpoint {
	case "", "/v1/chat/completions", "/chat/completions", "/v1/completions", "/v1/messages", "/v1/responses":
		return true
	}
	return false
}

// defaultMaxTokensFor picks an output ceiling for a discovered model: the
// published one when there is one, otherwise a quarter of the context
// window capped at 32k, which keeps a 256k model from defaulting to a 64k
// answer.
func defaultMaxTokensFor(e modelEntry) int64 {
	if v := e.maxOutput(); v > 0 {
		return v
	}
	if cw := e.contextWindow(); cw > 0 {
		return min(cw/4, 32768)
	}
	return 0
}

// DiscoverModels fetches available models from the provider's /models endpoint.
// It uses the provided context for cancellation and timeout; callers should set
// a deadline (e.g. context.WithTimeout) to avoid blocking indefinitely.
// Models whose IDs already appear in cfg.ExistingModels are skipped —
// user-specified models take precedence.
func DiscoverModels(ctx context.Context, cfg Config, resolver Resolver) ([]catwalk.Model, error) {
	resp, err := doRequest(ctx, http.MethodGet, cfg.BaseURL, "/models", cfg.APIKey, cfg.ExtraHeaders, resolver, nil)
	if err != nil {
		return nil, fmt.Errorf("discover models for provider %s: %w", cfg.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discover models for provider %s: %s", cfg.ID, resp.Status)
	}

	var modelsResp modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&modelsResp); err != nil {
		return nil, fmt.Errorf("discover models for provider %s: %w", cfg.ID, err)
	}

	// Index the published entries so user-specified models can borrow
	// metadata they left out.
	published := make(map[string]modelEntry, len(modelsResp.Data))
	for _, e := range modelsResp.Data {
		published[e.ID] = e
	}

	// Start with user-specified models. They win on every field they set;
	// a zero context window or output ceiling is filled from the gateway,
	// which is what makes "list the id, let discovery size it" work.
	result := make([]catwalk.Model, len(cfg.ExistingModels))
	copy(result, cfg.ExistingModels)
	for i := range result {
		e, ok := published[result[i].ID]
		if !ok {
			continue
		}
		if result[i].ContextWindow == 0 {
			result[i].ContextWindow = e.contextWindow()
		}
		if result[i].DefaultMaxTokens == 0 {
			result[i].DefaultMaxTokens = defaultMaxTokensFor(e)
		}
	}

	// Append discovered chat models not already in the list.
	existing := make(map[string]struct{}, len(cfg.ExistingModels))
	for _, m := range cfg.ExistingModels {
		existing[m.ID] = struct{}{}
	}
	for _, e := range modelsResp.Data {
		if _, ok := existing[e.ID]; ok || !e.servesChat() {
			continue
		}
		name := e.Name
		if name == "" {
			name = e.ID
		}
		result = append(result, catwalk.Model{
			ID:               e.ID,
			Name:             name,
			ContextWindow:    e.contextWindow(),
			DefaultMaxTokens: defaultMaxTokensFor(e),
		})
	}

	return result, nil
}
