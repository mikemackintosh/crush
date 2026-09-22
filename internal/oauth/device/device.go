// Package device implements the OAuth 2.0 device authorization grant
// (RFC 8628) against any standards-compliant authorization server. It
// discovers the server from the protected resource (RFC 9728) or from an
// issuer URL (RFC 8414, OpenID Connect Discovery), registers Crush as a
// public client through dynamic client registration (RFC 7591) when no
// client ID is configured, and refreshes the resulting tokens with the
// plain refresh_token grant.
package device

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/charmbracelet/crush/internal/oauth"
)

// GrantType is the device authorization grant type identifier.
const GrantType = "urn:ietf:params:oauth:grant-type:device_code"

const (
	clientName = "Crush"
	clientURI  = "https://charm.land/crush"
	userAgent  = "crush"

	httpTimeout = 30 * time.Second
	maxBody     = 1 << 20

	// fallbackLifetime is assumed for an access token that carries no
	// expiry and cannot be refreshed. Marking such a token expired would
	// make every request attempt (and fail) a refresh first.
	fallbackLifetime = 365 * 24 * time.Hour
)

// Options configures a device flow against one provider.
type Options struct {
	// Issuer is the authorization server URL. When empty the server is
	// discovered from ResourceURL through RFC 9728.
	Issuer string
	// ResourceURL is the protected resource the token is for, normally
	// the provider's base URL.
	ResourceURL string
	// Scope is the space-separated scope string to request.
	Scope string
	// ClientID and ClientSecret name a pre-registered client. When
	// ClientID is empty a client is registered dynamically.
	ClientID     string
	ClientSecret string
	// Client is a registration from an earlier login. It is reused when
	// its token endpoint still matches the discovered server so a fresh
	// login does not register yet another client.
	Client *oauth.OAuthClient
}

// Metadata is the subset of authorization server metadata (RFC 8414)
// the device flow needs.
type Metadata struct {
	Issuer                            string   `json:"issuer"`
	DeviceAuthorizationEndpoint       string   `json:"device_authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
}

// Authorization is what the user needs to complete the flow.
type Authorization struct {
	DeviceCode      string
	UserCode        string
	VerificationURL string
	ExpiresIn       int
	Interval        int
}

// Flow is one device authorization attempt.
type Flow struct {
	cfg    *oauth2.Config
	client *oauth.OAuthClient
	resp   *oauth2.DeviceAuthResponse
	// resource is the RFC 8707 resource indicator sent with every token
	// request so the issued token carries the provider's audience.
	resource string
}

// NewFlow discovers the authorization server and resolves the client to
// use, registering one when needed.
func NewFlow(ctx context.Context, opts Options) (*Flow, error) {
	issuer := opts.Issuer
	resource := strings.TrimSuffix(opts.ResourceURL, "/")
	if issuer == "" {
		if opts.ResourceURL == "" {
			return nil, errors.New("device flow needs an issuer or a resource URL")
		}
		prm, err := resolveProtectedResource(ctx, opts.ResourceURL)
		if err != nil {
			return nil, err
		}
		issuer = prm.issuer
		if prm.resource != "" {
			resource = prm.resource
		}
	}

	meta, err := Discover(ctx, issuer)
	if err != nil {
		return nil, err
	}

	client, err := resolveClient(ctx, meta, opts)
	if err != nil {
		return nil, err
	}
	client.Resource = resource

	cfg := &oauth2.Config{
		ClientID:     client.ClientID,
		ClientSecret: client.ClientSecret,
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: meta.DeviceAuthorizationEndpoint,
			TokenURL:      meta.TokenEndpoint,
			AuthStyle:     oauth2.AuthStyle(client.AuthStyle),
		},
	}
	if opts.Scope != "" {
		cfg.Scopes = strings.Fields(opts.Scope)
	}
	return &Flow{cfg: cfg, client: client, resource: resource}, nil
}

// Client returns the client registration the flow uses.
func (f *Flow) Client() *oauth.OAuthClient {
	return f.client
}

// Start requests a device code and returns what the user must do.
func (f *Flow) Start(ctx context.Context) (*Authorization, error) {
	resp, err := f.cfg.DeviceAuth(withClient(ctx), f.authOpts()...)
	if err != nil {
		return nil, fmt.Errorf("device authorization request: %w", err)
	}
	f.resp = resp
	expiresIn := 0
	if !resp.Expiry.IsZero() {
		expiresIn = int(time.Until(resp.Expiry).Seconds())
	}
	return &Authorization{
		DeviceCode:      resp.DeviceCode,
		UserCode:        resp.UserCode,
		VerificationURL: cmp.Or(resp.VerificationURIComplete, resp.VerificationURI),
		ExpiresIn:       expiresIn,
		Interval:        int(resp.Interval),
	}, nil
}

// Wait polls the token endpoint until the user approves, denies, or the
// device code expires.
func (f *Flow) Wait(ctx context.Context) (*oauth.Token, error) {
	if f.resp == nil {
		return nil, errors.New("device flow not started")
	}
	tok, err := f.cfg.DeviceAccessToken(withClient(ctx), f.resp, f.authOpts()...)
	if err != nil {
		return nil, fmt.Errorf("device token request: %w", err)
	}
	return fromOAuth2(tok, f.client), nil
}

// authOpts returns the extra parameters for device and token requests.
func (f *Flow) authOpts() []oauth2.AuthCodeOption {
	if f.resource == "" {
		return nil
	}
	return []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("resource", f.resource)}
}

// protectedResource is what RFC 9728 discovery yields.
type protectedResource struct {
	issuer   string
	resource string
}

// ResolveIssuer finds the authorization server that protects resourceURL
// through its protected resource metadata (RFC 9728).
func ResolveIssuer(ctx context.Context, resourceURL string) (string, error) {
	prm, err := resolveProtectedResource(ctx, resourceURL)
	if err != nil {
		return "", err
	}
	return prm.issuer, nil
}

func resolveProtectedResource(ctx context.Context, resourceURL string) (protectedResource, error) {
	u, err := parseHTTPURL(resourceURL)
	if err != nil {
		return protectedResource{}, fmt.Errorf("resource url: %w", err)
	}
	origin := u.Scheme + "://" + u.Host
	path := strings.TrimSuffix(u.Path, "/")

	candidates := []string{origin + "/.well-known/oauth-protected-resource"}
	if path != "" {
		candidates = []string{
			origin + "/.well-known/oauth-protected-resource" + path,
			origin + path + "/.well-known/oauth-protected-resource",
			origin + "/.well-known/oauth-protected-resource",
		}
	}

	var lastErr error
	for _, c := range candidates {
		var prm struct {
			Resource             string   `json:"resource"`
			AuthorizationServers []string `json:"authorization_servers"`
		}
		if err := getJSON(ctx, c, &prm); err != nil {
			lastErr = err
			continue
		}
		for _, as := range prm.AuthorizationServers {
			if _, err := parseHTTPURL(as); err == nil {
				return protectedResource{issuer: as, resource: prm.Resource}, nil
			}
		}
		lastErr = fmt.Errorf("%s lists no usable authorization server", c)
	}
	return protectedResource{}, fmt.Errorf("discover authorization server for %s: %w", resourceURL, lastErr)
}

// Discover fetches the authorization server metadata for issuer, trying
// RFC 8414 first and OpenID Connect Discovery second.
func Discover(ctx context.Context, issuer string) (*Metadata, error) {
	u, err := parseHTTPURL(issuer)
	if err != nil {
		return nil, fmt.Errorf("issuer url: %w", err)
	}
	origin := u.Scheme + "://" + u.Host
	path := strings.TrimSuffix(u.Path, "/")

	candidates := []string{
		origin + "/.well-known/oauth-authorization-server" + path,
		origin + path + "/.well-known/openid-configuration",
	}
	if path != "" {
		candidates = append(candidates,
			origin+"/.well-known/oauth-authorization-server",
			origin+"/.well-known/openid-configuration",
		)
	}

	var lastErr error
	for _, c := range candidates {
		var meta Metadata
		if err := getJSON(ctx, c, &meta); err != nil {
			lastErr = err
			continue
		}
		if meta.TokenEndpoint == "" {
			lastErr = fmt.Errorf("%s has no token_endpoint", c)
			continue
		}
		if meta.DeviceAuthorizationEndpoint == "" {
			return nil, fmt.Errorf("authorization server %s does not advertise a device_authorization_endpoint (RFC 8628)", issuer)
		}
		for _, ep := range []string{meta.TokenEndpoint, meta.DeviceAuthorizationEndpoint, meta.RegistrationEndpoint} {
			if ep == "" {
				continue
			}
			if _, err := parseHTTPURL(ep); err != nil {
				return nil, fmt.Errorf("authorization server %s: endpoint %q: %w", issuer, ep, err)
			}
		}
		return &meta, nil
	}
	return nil, fmt.Errorf("discover authorization server metadata for %s: %w", issuer, lastErr)
}

// placeholderRedirectURI satisfies authorization servers that insist on
// redirect_uris for every registration. A device-flow client never
// redirects, so the loopback address is inert.
const placeholderRedirectURI = "http://127.0.0.1/callback"

// Register registers Crush as a public device-flow client (RFC 7591).
// redirect_uris is omitted first, as the spec only requires it for
// redirect-based grants; a server that rejects the request with
// invalid_redirect_uri gets one retry carrying a loopback placeholder.
func Register(ctx context.Context, meta *Metadata, scope string) (*oauth.OAuthClient, error) {
	if meta.RegistrationEndpoint == "" {
		return nil, fmt.Errorf("authorization server %s does not support dynamic client registration; set oauth_client_id", meta.Issuer)
	}

	client, err := register(ctx, meta, scope, nil)
	var regErr *registrationError
	if errors.As(err, &regErr) && regErr.Code == "invalid_redirect_uri" {
		slog.Debug("Registration requires redirect_uris; retrying with loopback placeholder", "issuer", meta.Issuer)
		client, err = register(ctx, meta, scope, []string{placeholderRedirectURI})
	}
	return client, err
}

// registrationError is a registration response with an RFC 7591 error
// body.
type registrationError struct {
	StatusCode int
	Code       string
	Body       string
}

func (e *registrationError) Error() string {
	return fmt.Sprintf("client registration failed: status %d body %q", e.StatusCode, e.Body)
}

func register(ctx context.Context, meta *Metadata, scope string, redirectURIs []string) (*oauth.OAuthClient, error) {
	body, err := json.Marshal(struct {
		ClientName              string   `json:"client_name"`
		ClientURI               string   `json:"client_uri"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
		RedirectURIs            []string `json:"redirect_uris,omitempty"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		Scope                   string   `json:"scope,omitempty"`
	}{
		ClientName:              clientName,
		ClientURI:               clientURI,
		GrantTypes:              []string{GrantType, "refresh_token"},
		ResponseTypes:           []string{},
		RedirectURIs:            redirectURIs,
		TokenEndpointAuthMethod: "none",
		Scope:                   scope,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal registration request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.RegistrationEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create registration request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("client registration: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("read registration response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		var ep struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &ep)
		return nil, &registrationError{StatusCode: resp.StatusCode, Code: ep.Error, Body: string(data)}
	}

	var reg struct {
		ClientID                string   `json:"client_id"`
		ClientSecret            string   `json:"client_secret"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		GrantTypes              []string `json:"grant_types"`
	}
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("unmarshal registration response: %w", err)
	}
	if reg.ClientID == "" {
		return nil, errors.New("client registration response has no client_id")
	}
	// Servers may quietly narrow the grant types they hand out. Fail here
	// with the reason instead of at the device request with a bare
	// unauthorized_client.
	if len(reg.GrantTypes) > 0 && !slices.Contains(reg.GrantTypes, GrantType) {
		return nil, fmt.Errorf("authorization server %s registered the client without the device_code grant (granted: %s); allow it for dynamic registrations or set oauth_client_id to a client that has it", meta.Issuer, strings.Join(reg.GrantTypes, ", "))
	}

	return &oauth.OAuthClient{
		ClientID:     reg.ClientID,
		ClientSecret: reg.ClientSecret,
		AuthURL:      meta.DeviceAuthorizationEndpoint,
		TokenURL:     meta.TokenEndpoint,
		AuthStyle:    int(authStyle(reg.TokenEndpointAuthMethod, reg.ClientSecret != "")),
	}, nil
}

// RefreshToken exchanges the token's refresh token at the token endpoint
// recorded in its client registration. A response without a new refresh
// token keeps the old one.
func RefreshToken(ctx context.Context, tok *oauth.Token) (*oauth.Token, error) {
	if tok == nil || tok.Client == nil || tok.Client.TokenURL == "" {
		return nil, errors.New("token has no client registration to refresh with")
	}
	if tok.RefreshToken == "" {
		// Reported as invalid_grant so callers treat it like a revoked
		// refresh token and ask the user to sign in again.
		return nil, &oauth.TokenExchangeError{StatusCode: 0, Body: "invalid_grant: token has no refresh token"}
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", tok.RefreshToken)
	if tok.Client.Resource != "" {
		form.Set("resource", tok.Client.Resource)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tok.Client.TokenURL, strings.NewReader(""))
	if err != nil {
		return nil, fmt.Errorf("create refresh request: %w", err)
	}
	if oauth2.AuthStyle(tok.Client.AuthStyle) == oauth2.AuthStyleInHeader && tok.Client.ClientSecret != "" {
		req.SetBasicAuth(url.QueryEscape(tok.Client.ClientID), url.QueryEscape(tok.Client.ClientSecret))
	} else {
		form.Set("client_id", tok.Client.ClientID)
		if tok.Client.ClientSecret != "" {
			form.Set("client_secret", tok.Client.ClientSecret)
		}
	}
	encoded := form.Encode()
	req.Body = io.NopCloser(strings.NewReader(encoded))
	req.ContentLength = int64(len(encoded))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("read refresh response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &oauth.TokenExchangeError{StatusCode: resp.StatusCode, Body: string(data)}
	}

	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &tr); err != nil {
		return nil, fmt.Errorf("unmarshal refresh response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, errors.New("refresh response has no access_token")
	}

	next := &oauth.Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: cmp.Or(tr.RefreshToken, tok.RefreshToken),
		IDToken:      tr.IDToken,
		ExpiresIn:    int(tr.ExpiresIn),
		Client:       tok.Client,
	}
	setExpiry(next)
	return next, nil
}

// resolveClient picks the client registration for opts: a configured
// client ID, then a saved registration for the same token endpoint, then
// a fresh dynamic registration.
func resolveClient(ctx context.Context, meta *Metadata, opts Options) (*oauth.OAuthClient, error) {
	if opts.ClientID != "" {
		return &oauth.OAuthClient{
			ClientID:     opts.ClientID,
			ClientSecret: opts.ClientSecret,
			AuthURL:      meta.DeviceAuthorizationEndpoint,
			TokenURL:     meta.TokenEndpoint,
			AuthStyle:    int(authStyle("", opts.ClientSecret != "")),
		}, nil
	}
	if c := opts.Client; c != nil && c.ClientID != "" && c.TokenURL == meta.TokenEndpoint {
		reuse := *c
		reuse.AuthURL = meta.DeviceAuthorizationEndpoint
		return &reuse, nil
	}
	return Register(ctx, meta, opts.Scope)
}

// authStyle maps a token_endpoint_auth_method to the oauth2 client
// authentication style. Public clients ("none") send client_id in the
// form body; that is also the safe default when the server left the
// method unspecified and issued no secret.
func authStyle(method string, hasSecret bool) oauth2.AuthStyle {
	switch method {
	case "client_secret_basic":
		return oauth2.AuthStyleInHeader
	case "client_secret_post", "none":
		return oauth2.AuthStyleInParams
	}
	if hasSecret {
		return oauth2.AuthStyleInHeader
	}
	return oauth2.AuthStyleInParams
}

func fromOAuth2(t *oauth2.Token, client *oauth.OAuthClient) *oauth.Token {
	tok := &oauth.Token{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		Client:       client,
	}
	if id, ok := t.Extra("id_token").(string); ok {
		tok.IDToken = id
	}
	if !t.Expiry.IsZero() {
		tok.ExpiresAt = t.Expiry.Unix()
		tok.SetExpiresIn()
		return tok
	}
	setExpiry(tok)
	return tok
}

// setExpiry derives ExpiresAt from ExpiresIn. A token with neither an
// expiry nor a refresh token is given a long fallback lifetime instead
// of being marked expired, because a refresh could never succeed.
func setExpiry(tok *oauth.Token) {
	if tok.ExpiresIn <= 0 && tok.RefreshToken == "" {
		slog.Warn("OAuth token has no expiry and no refresh token; assuming a long lifetime")
		tok.ExpiresAt = time.Now().Add(fallbackLifetime).Unix()
		tok.SetExpiresIn()
		return
	}
	tok.SetExpiresAt()
}

func getJSON(ctx context.Context, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", rawURL, resp.StatusCode)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("GET %s: %w", rawURL, err)
	}
	return nil
}

// parseHTTPURL accepts https URLs, and plain http only for loopback
// hosts, so a spoofed discovery document cannot redirect tokens to a
// cleartext endpoint.
func parseHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%q has no host", raw)
	}
	switch u.Scheme {
	case "https":
		return u, nil
	case "http":
		host := u.Hostname()
		if host == "localhost" || strings.HasSuffix(host, ".localhost") {
			return u, nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return u, nil
		}
		return nil, fmt.Errorf("%q must use https (http is allowed for loopback only)", raw)
	default:
		return nil, fmt.Errorf("%q must be an http(s) url", raw)
	}
}

func httpClient() *http.Client {
	return &http.Client{Timeout: httpTimeout}
}

func withClient(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, httpClient())
}
