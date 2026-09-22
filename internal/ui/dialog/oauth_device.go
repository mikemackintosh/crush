package dialog

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/oauth/device"
	"github.com/charmbracelet/crush/internal/ui/common"
)

// NewOAuthDevice opens the device-flow dialog for a custom provider whose
// config enables OAuth device authorization.
func NewOAuthDevice(
	com *common.Common,
	isOnboarding bool,
	provider catwalk.Provider,
	model config.SelectedModel,
	modelType config.SelectedModelType,
	opts device.Options,
) (*OAuth, tea.Cmd) {
	return newOAuth(com, isOnboarding, provider, model, modelType, &OAuthDevice{
		displayName: provider.Name,
		opts:        opts,
	})
}

// OAuthDevice drives the standard OAuth 2.0 device authorization flow
// (RFC 8628) for the OAuth dialog.
type OAuthDevice struct {
	displayName string
	opts        device.Options
	flow        *device.Flow
	cancelFunc  func()
}

var _ OAuthProvider = (*OAuthDevice)(nil)

func (m *OAuthDevice) name() string {
	return m.displayName
}

func (m *OAuthDevice) initiateAuth() tea.Msg {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	flow, err := device.NewFlow(ctx, m.opts)
	if err != nil {
		return ActionOAuthErrored{fmt.Errorf("failed to set up device auth: %w", err)}
	}
	auth, err := flow.Start(ctx)
	if err != nil {
		return ActionOAuthErrored{fmt.Errorf("failed to initiate device auth: %w", err)}
	}
	m.flow = flow

	return ActionInitiateOAuth{
		DeviceCode:      auth.DeviceCode,
		UserCode:        auth.UserCode,
		ExpiresIn:       auth.ExpiresIn,
		VerificationURL: auth.VerificationURL,
		Interval:        auth.Interval,
	}
}

func (m *OAuthDevice) startPolling(_ string, _ int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(context.Background())
		m.cancelFunc = cancel

		token, err := m.flow.Wait(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return ActionOAuthErrored{err}
		}
		return ActionCompleteOAuth{token}
	}
}

func (m *OAuthDevice) stopPolling() tea.Msg {
	if m.cancelFunc != nil {
		m.cancelFunc()
	}
	return nil
}
