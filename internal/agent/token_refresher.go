package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/charmbracelet/crush/internal/config"
)

// tokenRefreshInterval is how often the background refresher looks at the
// OAuth tokens of the providers in use. Token.IsExpired already reports
// true a little before the real expiry, so a one minute cadence renews a
// token before a request can be sent with it.
const tokenRefreshInterval = time.Minute

// startTokenRefresher renews expiring OAuth tokens in the background, so a
// session that sat idle past a token's lifetime does not pay for it with a
// 401 and a retry on its next prompt. Only the providers behind the
// configured large and small models are watched.
func (c *coordinator) startTokenRefresher(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(tokenRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.refreshExpiringTokens(ctx)
			}
		}
	}()
}

// refreshExpiringTokens refreshes every in-use provider whose OAuth token
// is expired or about to be. Failures are logged and retried on the next
// tick; the 401 path still covers the request that slips through.
func (c *coordinator) refreshExpiringTokens(ctx context.Context) {
	cfg := c.cfg.Config()
	if cfg == nil {
		return
	}
	seen := map[string]bool{}
	for _, modelType := range []config.SelectedModelType{config.SelectedModelTypeLarge, config.SelectedModelTypeSmall} {
		selected, ok := cfg.Models[modelType]
		if !ok || seen[selected.Provider] {
			continue
		}
		seen[selected.Provider] = true
		pc, ok := cfg.Providers.Get(selected.Provider)
		if !ok || pc.OAuthToken == nil || !pc.OAuthToken.IsExpired() {
			continue
		}
		slog.Info("OAuth token expiring; refreshing in the background", "provider", pc.ID)
		if err := c.refreshOAuth2Token(ctx, pc); err != nil {
			slog.Warn("Background OAuth refresh failed; will retry", "provider", pc.ID, "error", err)
		}
	}
}
