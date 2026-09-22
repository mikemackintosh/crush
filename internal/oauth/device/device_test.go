package device

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/charmbracelet/crush/internal/oauth"
)

// fakeServer is a minimal OAuth authorization server plus protected
// resource that speaks RFC 9728, RFC 8414, RFC 7591 and RFC 8628.
type fakeServer struct {
	srv *httptest.Server

	registrations atomic.Int32
	polls         atomic.Int32
	// pendingPolls is how many token polls answer authorization_pending
	// before the token is issued.
	pendingPolls int32
	// oidcOnly serves metadata only at the OpenID Connect path.
	oidcOnly bool
	// noDevice omits device_authorization_endpoint from the metadata.
	noDevice bool
	// refreshAuth records the Authorization header of the last refresh.
	refreshAuth string
	// refreshForm records the form of the last refresh.
	refreshForm map[string]string
	// refreshStatus overrides the refresh response when non-zero.
	refreshStatus int
	// rotateRefresh returns a new refresh token on refresh.
	rotateRefresh bool
	// requireRedirect rejects registrations without redirect_uris, as some
	// servers do even for device-flow clients.
	requireRedirect bool
	// lastRedirectURIs records the redirect_uris of the last registration.
	lastRedirectURIs []any
	// dropDeviceGrant answers registrations with only the code grant.
	dropDeviceGrant bool
	// deviceResource and tokenResource record the RFC 8707 resource
	// parameter of the last device and token requests.
	deviceResource, tokenResource string
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{pendingPolls: 1}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) url(p string) string { return f.srv.URL + p }

func (f *fakeServer) metadata() Metadata {
	m := Metadata{
		Issuer:                            f.srv.URL,
		TokenEndpoint:                     f.url("/token"),
		RegistrationEndpoint:              f.url("/register"),
		DeviceAuthorizationEndpoint:       f.url("/device"),
		TokenEndpointAuthMethodsSupported: []string{"none"},
		GrantTypesSupported:               []string{GrantType, "refresh_token"},
	}
	if f.noDevice {
		m.DeviceAuthorizationEndpoint = ""
	}
	return m
}

func (f *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	writeJSON := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource/v1":
		writeJSON(200, map[string]any{
			"resource":              f.url("/v1"),
			"authorization_servers": []string{f.srv.URL},
		})
	case "/.well-known/oauth-authorization-server":
		if f.oidcOnly {
			http.NotFound(w, r)
			return
		}
		writeJSON(200, f.metadata())
	case "/.well-known/openid-configuration":
		writeJSON(200, f.metadata())
	case "/register":
		f.registrations.Add(1)
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		grants, _ := req["grant_types"].([]any)
		if len(grants) == 0 || grants[0] != GrantType {
			writeJSON(400, map[string]string{"error": "invalid_client_metadata"})
			return
		}
		f.lastRedirectURIs, _ = req["redirect_uris"].([]any)
		if f.requireRedirect && len(f.lastRedirectURIs) == 0 {
			writeJSON(400, map[string]string{"error": "invalid_redirect_uri", "error_description": "between 1 and 10 redirect_uris required"})
			return
		}
		granted := []string{GrantType, "refresh_token"}
		if f.dropDeviceGrant {
			granted = []string{"authorization_code", "refresh_token"}
		}
		writeJSON(201, map[string]any{
			"client_id":                  "dyn-client",
			"token_endpoint_auth_method": "none",
			"grant_types":                granted,
		})
	case "/device":
		_ = r.ParseForm()
		f.deviceResource = r.Form.Get("resource")
		if r.Form.Get("client_id") == "" {
			writeJSON(401, map[string]string{"error": "invalid_client"})
			return
		}
		writeJSON(200, map[string]any{
			"device_code":               "dev-123",
			"user_code":                 "ABCD-EFGH",
			"verification_uri":          f.url("/activate"),
			"verification_uri_complete": f.url("/activate?user_code=ABCD-EFGH"),
			"expires_in":                600,
			"interval":                  1,
		})
	case "/token":
		_ = r.ParseForm()
		f.tokenResource = r.Form.Get("resource")
		switch r.Form.Get("grant_type") {
		case GrantType:
			if r.Form.Get("device_code") != "dev-123" {
				writeJSON(400, map[string]string{"error": "invalid_grant"})
				return
			}
			if f.polls.Add(1) <= f.pendingPolls {
				writeJSON(400, map[string]string{"error": "authorization_pending"})
				return
			}
			writeJSON(200, map[string]any{
				"access_token":  "access-1",
				"refresh_token": "refresh-1",
				"token_type":    "Bearer",
				"expires_in":    3600,
			})
		case "refresh_token":
			f.refreshAuth = r.Header.Get("Authorization")
			f.refreshForm = map[string]string{}
			for k := range r.Form {
				f.refreshForm[k] = r.Form.Get(k)
			}
			if f.refreshStatus != 0 {
				writeJSON(f.refreshStatus, map[string]string{"error": "invalid_grant", "error_description": "revoked"})
				return
			}
			resp := map[string]any{
				"access_token": "access-2",
				"token_type":   "Bearer",
				"expires_in":   1800,
			}
			if f.rotateRefresh {
				resp["refresh_token"] = "refresh-2"
			}
			writeJSON(200, resp)
		default:
			writeJSON(400, map[string]string{"error": "unsupported_grant_type"})
		}
	default:
		http.NotFound(w, r)
	}
}

func TestFlow_DiscoverRegisterAndAuthorize(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	ctx := context.Background()

	flow, err := NewFlow(ctx, Options{ResourceURL: f.url("/v1"), Scope: "llm openid"})
	require.NoError(t, err)
	require.Equal(t, int32(1), f.registrations.Load())
	require.Equal(t, "dyn-client", flow.Client().ClientID)
	require.Equal(t, f.url("/token"), flow.Client().TokenURL)
	require.Equal(t, []string{"llm", "openid"}, flow.cfg.Scopes)

	auth, err := flow.Start(ctx)
	require.NoError(t, err)
	require.Equal(t, "ABCD-EFGH", auth.UserCode)
	require.Equal(t, f.url("/activate?user_code=ABCD-EFGH"), auth.VerificationURL)
	require.InDelta(t, 600, auth.ExpiresIn, 5)

	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tok, err := flow.Wait(waitCtx)
	require.NoError(t, err)
	require.Equal(t, "access-1", tok.AccessToken)
	require.Equal(t, "refresh-1", tok.RefreshToken)
	require.False(t, tok.IsExpired())
	require.NotNil(t, tok.Client)
	require.Equal(t, "dyn-client", tok.Client.ClientID)
	require.GreaterOrEqual(t, f.polls.Load(), int32(2), "first poll must be answered authorization_pending")
	require.Equal(t, f.url("/v1"), f.deviceResource, "RFC 8707 resource from protected resource metadata")
	require.Equal(t, f.url("/v1"), f.tokenResource)
	require.Equal(t, f.url("/v1"), tok.Client.Resource)
}

func TestRegister_RejectsClientWithoutDeviceGrant(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	f.dropDeviceGrant = true

	meta := f.metadata()
	_, err := Register(context.Background(), &meta, "")
	require.ErrorContains(t, err, "without the device_code grant")
	require.ErrorContains(t, err, "authorization_code, refresh_token")
}

func TestRefreshToken_SendsResource(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)

	tok := &oauth.Token{
		RefreshToken: "refresh-1",
		Client:       &oauth.OAuthClient{ClientID: "dyn-client", TokenURL: f.url("/token"), Resource: "https://gw.example"},
	}
	_, err := RefreshToken(context.Background(), tok)
	require.NoError(t, err)
	require.Equal(t, "https://gw.example", f.refreshForm["resource"])
}

func TestNewFlow_ReusesSavedRegistration(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)

	saved := &oauth.OAuthClient{ClientID: "saved", TokenURL: f.url("/token"), AuthStyle: int(oauth2.AuthStyleInParams)}
	flow, err := NewFlow(context.Background(), Options{Issuer: f.srv.URL, Client: saved})
	require.NoError(t, err)
	require.Equal(t, int32(0), f.registrations.Load())
	require.Equal(t, "saved", flow.Client().ClientID)
	require.Equal(t, f.url("/device"), flow.Client().AuthURL)
}

func TestNewFlow_StaleRegistrationIsReplaced(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)

	saved := &oauth.OAuthClient{ClientID: "saved", TokenURL: "https://other.example/token"}
	flow, err := NewFlow(context.Background(), Options{Issuer: f.srv.URL, Client: saved})
	require.NoError(t, err)
	require.Equal(t, int32(1), f.registrations.Load())
	require.Equal(t, "dyn-client", flow.Client().ClientID)
}

func TestNewFlow_PreRegisteredClientSkipsRegistration(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)

	flow, err := NewFlow(context.Background(), Options{Issuer: f.srv.URL, ClientID: "mine", ClientSecret: "s3cret"})
	require.NoError(t, err)
	require.Equal(t, int32(0), f.registrations.Load())
	require.Equal(t, "mine", flow.Client().ClientID)
	require.Equal(t, int(oauth2.AuthStyleInHeader), flow.Client().AuthStyle)
}

func TestDiscover_FallsBackToOpenIDConfiguration(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	f.oidcOnly = true

	meta, err := Discover(context.Background(), f.srv.URL)
	require.NoError(t, err)
	require.Equal(t, f.url("/device"), meta.DeviceAuthorizationEndpoint)
}

func TestDiscover_RejectsServerWithoutDeviceGrant(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	f.noDevice = true

	_, err := Discover(context.Background(), f.srv.URL)
	require.ErrorContains(t, err, "device_authorization_endpoint")
}

func TestRegister_RequiresRegistrationEndpoint(t *testing.T) {
	t.Parallel()
	_, err := Register(context.Background(), &Metadata{Issuer: "https://as.example", TokenEndpoint: "https://as.example/token"}, "")
	require.ErrorContains(t, err, "oauth_client_id")
}

func TestRefreshToken_PublicClient(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)

	tok := &oauth.Token{
		AccessToken:  "old",
		RefreshToken: "refresh-1",
		Client:       &oauth.OAuthClient{ClientID: "dyn-client", TokenURL: f.url("/token"), AuthStyle: int(oauth2.AuthStyleInParams)},
	}
	next, err := RefreshToken(context.Background(), tok)
	require.NoError(t, err)
	require.Equal(t, "access-2", next.AccessToken)
	require.Equal(t, "refresh-1", next.RefreshToken, "old refresh token kept when none is issued")
	require.Equal(t, 1800, next.ExpiresIn)
	require.False(t, next.IsExpired())
	require.Same(t, tok.Client, next.Client)
	require.Equal(t, "dyn-client", f.refreshForm["client_id"])
	require.Empty(t, f.refreshAuth)
}

func TestRefreshToken_ConfidentialClientUsesBasicAuth(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	f.rotateRefresh = true

	tok := &oauth.Token{
		RefreshToken: "refresh-1",
		Client: &oauth.OAuthClient{
			ClientID: "conf", ClientSecret: "s3cret",
			TokenURL: f.url("/token"), AuthStyle: int(oauth2.AuthStyleInHeader),
		},
	}
	next, err := RefreshToken(context.Background(), tok)
	require.NoError(t, err)
	require.Equal(t, "refresh-2", next.RefreshToken)
	require.True(t, strings.HasPrefix(f.refreshAuth, "Basic "))
	require.Empty(t, f.refreshForm["client_secret"])
}

func TestRefreshToken_RevokedIsTokenExchangeError(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	f.refreshStatus = 400

	tok := &oauth.Token{
		RefreshToken: "refresh-1",
		Client:       &oauth.OAuthClient{ClientID: "dyn-client", TokenURL: f.url("/token")},
	}
	_, err := RefreshToken(context.Background(), tok)
	var exchangeErr *oauth.TokenExchangeError
	require.ErrorAs(t, err, &exchangeErr)
	require.True(t, exchangeErr.IsRefreshTokenRevoked())
}

func TestRefreshToken_WithoutRefreshTokenAsksForReauth(t *testing.T) {
	t.Parallel()
	_, err := RefreshToken(context.Background(), &oauth.Token{Client: &oauth.OAuthClient{TokenURL: "https://as.example/token"}})
	var exchangeErr *oauth.TokenExchangeError
	require.ErrorAs(t, err, &exchangeErr)
	require.True(t, exchangeErr.IsRefreshTokenRevoked())
}

func TestParseHTTPURL(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"https://as.example/x", "http://localhost:8080", "http://127.0.0.1:9", "http://[::1]:9"} {
		_, err := parseHTTPURL(ok)
		require.NoError(t, err, ok)
	}
	for _, bad := range []string{"http://as.example", "ftp://as.example", "as.example", ""} {
		_, err := parseHTTPURL(bad)
		require.Error(t, err, bad)
	}
}

func TestSetExpiry_NoExpiryNoRefreshGetsFallback(t *testing.T) {
	t.Parallel()
	tok := &oauth.Token{AccessToken: "a"}
	setExpiry(tok)
	require.False(t, tok.IsExpired())

	withRefresh := &oauth.Token{AccessToken: "a", RefreshToken: "r"}
	setExpiry(withRefresh)
	require.True(t, withRefresh.IsExpired(), "refreshable token without expiry is refreshed eagerly")
}

func TestRegister_OmitsRedirectURIsByDefault(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)

	meta := f.metadata()
	_, err := Register(context.Background(), &meta, "")
	require.NoError(t, err)
	require.Equal(t, int32(1), f.registrations.Load())
	require.Empty(t, f.lastRedirectURIs)
}

func TestRegister_RetriesWithLoopbackRedirectWhenRequired(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	f.requireRedirect = true

	meta := f.metadata()
	client, err := Register(context.Background(), &meta, "")
	require.NoError(t, err)
	require.Equal(t, "dyn-client", client.ClientID)
	require.Equal(t, int32(2), f.registrations.Load(), "one bare attempt, one retry")
	require.Equal(t, []any{placeholderRedirectURI}, f.lastRedirectURIs)
}

func TestRegister_OtherErrorsAreNotRetried(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)

	meta := f.metadata()
	meta.RegistrationEndpoint = f.url("/missing")
	_, err := Register(context.Background(), &meta, "")
	var regErr *registrationError
	require.ErrorAs(t, err, &regErr)
	require.Equal(t, 404, regErr.StatusCode)
	require.Equal(t, int32(0), f.registrations.Load())
}
