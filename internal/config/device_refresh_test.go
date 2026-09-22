package config

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/env"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestRefreshOAuthToken_DeviceFlowProvider verifies that a custom provider
// whose token carries its own client registration is refreshed at that
// registration's token endpoint, and that the new credential is persisted
// to disk for both the oauth block and the mirrored api_key.
func TestRefreshOAuthToken_DeviceFlowProvider(t *testing.T) {
	var gotForm map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		gotForm = map[string]string{}
		for k := range r.Form {
			gotForm[k] = r.Form.Get(k)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at1",
			"refresh_token": "rt1",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(srv.Close)

	expired := &oauth.Token{
		AccessToken:  "at0",
		RefreshToken: "rt0",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
		Client:       &oauth.OAuthClient{ClientID: "dyn-client", TokenURL: srv.URL + "/token"},
	}

	configPath := filepath.Join(t.TempDir(), "crush.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"mygw":{"base_url":"https://gw.example/v1","oauth_device":true}}}`), 0o600))

	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("mygw", ProviderConfig{
		ID:          "mygw",
		Name:        "My Gateway",
		BaseURL:     "https://gw.example/v1",
		OAuthDevice: true,
		APIKey:      expired.AccessToken,
		OAuthToken:  expired,
	})
	store := &ConfigStore{
		config:         &Config{Providers: providers},
		globalDataPath: configPath,
		// No working directory: the post-write auto-reload would otherwise
		// read the real user config instead of the temp file.
		workingDir: "",
	}

	require.NoError(t, store.RefreshOAuthToken(context.Background(), ScopeGlobal, "mygw"))

	require.Equal(t, "refresh_token", gotForm["grant_type"])
	require.Equal(t, "rt0", gotForm["refresh_token"])
	require.Equal(t, "dyn-client", gotForm["client_id"])

	pc, ok := store.Config().Providers.Get("mygw")
	require.True(t, ok)
	require.Equal(t, "at1", pc.APIKey)
	require.Equal(t, "at1", pc.OAuthToken.AccessToken)
	require.Equal(t, "rt1", pc.OAuthToken.RefreshToken)
	require.NotNil(t, pc.OAuthToken.Client, "client registration must survive a refresh")
	require.False(t, pc.OAuthToken.IsExpired())

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, "at1", gjson.GetBytes(data, "providers.mygw.api_key").String())
	require.Equal(t, "at1", gjson.GetBytes(data, "providers.mygw.oauth.access_token").String())
	require.Equal(t, "dyn-client", gjson.GetBytes(data, "providers.mygw.oauth.client.client_id").String())
	require.True(t, gjson.GetBytes(data, "providers.mygw.oauth_device").Bool(), "provider settings untouched")
}

func TestRefreshOAuthToken_UnknownProviderWithoutRegistration(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "crush.json")
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("mygw", ProviderConfig{
		ID:         "mygw",
		OAuthToken: &oauth.Token{AccessToken: "at0", RefreshToken: "rt0"},
	})
	store := &ConfigStore{
		config:         &Config{Providers: providers},
		globalDataPath: configPath,
		workingDir:     filepath.Dir(configPath),
	}
	err := store.RefreshOAuthToken(context.Background(), ScopeGlobal, "mygw")
	require.ErrorContains(t, err, "OAuth refresh not supported")
}

func TestProviderConfig_DeviceAuthOptions(t *testing.T) {
	t.Setenv("CRUSH_TEST_GW_CLIENT_ID", "cid-from-env")

	pc := ProviderConfig{
		BaseURL:       "https://gw.example/v1",
		OAuthIssuer:   "https://auth.example",
		OAuthScope:    "llm offline_access",
		OAuthClientID: "$CRUSH_TEST_GW_CLIENT_ID",
		OAuthToken:    &oauth.Token{Client: &oauth.OAuthClient{ClientID: "saved"}},
	}
	require.True(t, pc.UsesDeviceAuth())

	opts, err := pc.DeviceAuthOptions(NewShellVariableResolver(env.New()))
	require.NoError(t, err)
	require.Equal(t, "https://auth.example", opts.Issuer)
	require.Equal(t, "https://gw.example/v1", opts.ResourceURL)
	require.Equal(t, "llm offline_access", opts.Scope)
	require.Equal(t, "cid-from-env", opts.ClientID)
	require.Equal(t, "saved", opts.Client.ClientID)

	require.False(t, (&ProviderConfig{}).UsesDeviceAuth())
	require.True(t, (&ProviderConfig{OAuthDevice: true}).UsesDeviceAuth())
}
