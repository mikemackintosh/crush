package discover

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

func TestDiscoverModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/models", r.URL.Path)
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"object": "list",
			"data": [
				{"id": "model-a", "object": "model", "owned_by": "org"},
				{"id": "model-b", "object": "model", "owned_by": "org"}
			]
		}`))
	}))
	defer server.Close()

	cfg := Config{
		ID:      "test",
		BaseURL: server.URL + "/v1",
		APIKey:  "test-key",
	}

	models, err := DiscoverModels(context.Background(), cfg, &mockResolver{})
	require.NoError(t, err)
	require.Len(t, models, 2)
	require.Equal(t, "model-a", models[0].ID)
	require.Equal(t, "model-b", models[1].ID)
}

func TestDiscoverModels_ExistingModelsWin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"id": "model-a", "object": "model"},
				{"id": "model-b", "object": "model"}
			]
		}`))
	}))
	defer server.Close()

	cfg := Config{
		ID:      "test",
		BaseURL: server.URL + "/v1",
		APIKey:  "test-key",
		ExistingModels: []catwalk.Model{
			{ID: "model-a", Name: "My Custom Name", ContextWindow: 200000, CanReason: true},
		},
	}

	models, err := DiscoverModels(context.Background(), cfg, &mockResolver{})
	require.NoError(t, err)
	require.Len(t, models, 2)

	require.Equal(t, "model-a", models[0].ID)
	require.Equal(t, "My Custom Name", models[0].Name)
	require.Equal(t, int64(200000), models[0].ContextWindow)
	require.True(t, models[0].CanReason)

	require.Equal(t, "model-b", models[1].ID)
	require.Equal(t, "model-b", models[1].Name)
}

func TestDiscoverModels_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := Config{
		ID:      "test",
		BaseURL: server.URL + "/v1",
		APIKey:  "test-key",
	}

	models, err := DiscoverModels(context.Background(), cfg, &mockResolver{})
	require.Error(t, err)
	require.Nil(t, models)
}

func TestDiscoverModels_ExtraHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "custom-value", r.Header.Get("X-Custom"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "m1", "object": "model"}]}`))
	}))
	defer server.Close()

	cfg := Config{
		ID:      "test",
		BaseURL: server.URL + "/v1",
		APIKey:  "test-key",
		ExtraHeaders: map[string]string{
			"X-Custom": "custom-value",
		},
	}

	models, err := DiscoverModels(context.Background(), cfg, &mockResolver{})
	require.NoError(t, err)
	require.Len(t, models, 1)
}

type mockResolver struct{}

func (m *mockResolver) ResolveValue(val string) (string, error) { return val, nil }

type envResolver struct {
	env map[string]string
}

func (e *envResolver) ResolveValue(val string) (string, error) {
	if v, ok := e.env[val]; ok {
		return v, nil
	}
	return val, nil
}

func TestDiscoverModels_ResolvesShellVariables(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer resolved-key", r.Header.Get("Authorization"))
		require.Equal(t, "resolved-header", r.Header.Get("X-Custom"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "m1", "object": "model"}]}`))
	}))
	defer server.Close()

	cfg := Config{
		ID:      "test",
		BaseURL: server.URL + "/v1",
		APIKey:  "$MY_API_KEY",
		ExtraHeaders: map[string]string{
			"X-Custom": "$MY_HEADER",
		},
	}

	resolver := &envResolver{env: map[string]string{
		"$MY_API_KEY": "resolved-key",
		"$MY_HEADER":  "resolved-header",
	}}

	models, err := DiscoverModels(context.Background(), cfg, resolver)
	require.NoError(t, err)
	require.Len(t, models, 1)
}

func TestDiscoverModels_SkipsEmptyExtraHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("X-Empty"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "m1", "object": "model"}]}`))
	}))
	defer server.Close()

	cfg := Config{
		ID:      "test",
		BaseURL: server.URL + "/v1",
		APIKey:  "test-key",
		ExtraHeaders: map[string]string{
			"X-Empty": "$UNSET_VAR",
		},
	}

	resolver := &envResolver{env: map[string]string{
		"$UNSET_VAR": "",
	}}

	models, err := DiscoverModels(context.Background(), cfg, resolver)
	require.NoError(t, err)
	require.Len(t, models, 1)
}

func TestDiscoverModels_NoAuthWhenNoAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Empty(t, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "m1", "object": "model"}]}`))
	}))
	defer server.Close()

	cfg := Config{
		ID:      "test",
		BaseURL: server.URL + "/v1",
		APIKey:  "",
	}

	models, err := DiscoverModels(context.Background(), cfg, &mockResolver{})
	require.NoError(t, err)
	require.Len(t, models, 1)
}

func TestStripV1Suffix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"http://localhost:8000/v1", "http://localhost:8000"},
		{"http://localhost:8000/v1/", "http://localhost:8000"},
		{"http://localhost:8000", "http://localhost:8000"},
		{"http://localhost:8000/", "http://localhost:8000"},
		{"http://localhost:8000/api/v1", "http://localhost:8000/api"},
		{"", ""},
	}
	for _, tt := range tests {
		got := stripV1Suffix(tt.input)
		require.Equal(t, tt.want, got, "stripV1Suffix(%q)", tt.input)
	}
}

// A gateway in front of SGLang or vLLM publishes max_model_len and labels
// non-chat models with the endpoint they serve. Discovery reads the
// metadata, sizes the model from it, fills what a user-listed model left
// blank, and keeps speech models out of the picker.
func TestDiscoverModels_GatewayMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"RadixArk/Qwen3.8-27B-NVFP4","object":"model","owned_by":"sglang","max_model_len":262144},
			{"id":"Qwen/Qwen3-TTS","object":"model","owned_by":"vllm","endpoint":"/v1/audio/speech","max_model_len":4096},
			{"id":"router/small","object":"model","name":"Small","context_length":32000,"max_output_tokens":4000}
		]}`))
	}))
	defer server.Close()

	cfg := Config{
		ID:      "llmgw",
		BaseURL: server.URL,
		ExistingModels: []catwalk.Model{
			{ID: "RadixArk/Qwen3.8-27B-NVFP4", Name: "Qwen 3.8 27B"},
		},
	}
	models, err := DiscoverModels(context.Background(), cfg, &mockResolver{})
	require.NoError(t, err)
	require.Len(t, models, 2, "the speech model is not a chat model")

	require.Equal(t, "Qwen 3.8 27B", models[0].Name, "user fields win")
	require.Equal(t, int64(262144), models[0].ContextWindow, "context window filled from the gateway")
	require.Equal(t, int64(32768), models[0].DefaultMaxTokens, "a quarter of the window, capped")

	require.Equal(t, "router/small", models[1].ID)
	require.Equal(t, "Small", models[1].Name)
	require.Equal(t, int64(32000), models[1].ContextWindow)
	require.Equal(t, int64(4000), models[1].DefaultMaxTokens, "published output ceiling wins over the heuristic")
}
