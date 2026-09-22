package cmd

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/charmbracelet/crush/internal/client"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/logout"
	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"
)

// providerDisplayNames maps OAuth-capable provider IDs to display names.
// Keep this list in sync with the switch in RunE and the login command.
var providerDisplayNames = map[string]string{
	"hyper":   "Charm Hyper",
	"copilot": "GitHub Copilot",
	"openai":  "ChatGPT",
}

var logoutCmd = &cobra.Command{
	Aliases: []string{"signout"},
	Use:     "logout [platform]",
	Short:   "Logout Crush from a platform",
	Long: `Logout Crush from a specified platform, removing stored credentials.
The platform should be provided as an argument.
If no argument is given, a list of logged-in platforms will be shown.
Available platforms are: hyper, copilot, openai, or the ID of any custom
provider signed in through the OAuth device flow.`,
	Example: `
# Sign out from Charm Hyper
crush logout hyper

# Sign out from GitHub Copilot
crush logout copilot

# Sign out from your ChatGPT (OpenAI) account
crush logout openai

# Sign out from a custom provider that uses the OAuth device flow
crush logout my-gateway
  `,
	ValidArgs: []cobra.Completion{
		"hyper",
		"copilot",
		"github",
		"github-copilot",
		"openai",
		"chatgpt",
	},
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, ws, cleanup, err := connectToServer(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		progressEnabled := ws.Config.Options.Progress == nil || *ws.Config.Options.Progress
		if progressEnabled && supportsProgressBar() {
			_, _ = fmt.Fprintf(os.Stderr, ansi.SetIndeterminateProgressBar)
			defer func() { _, _ = fmt.Fprintf(os.Stderr, ansi.ResetProgressBar) }()
		}

		var provider string
		chose := false
		if len(args) == 0 {
			provider, chose, err = pickLoggedInProvider(c, ws.ID)
			if err != nil {
				return err
			}
			if provider == "" {
				return nil
			}
		} else {
			provider = args[0]
		}

		// Canonicalize aliases before prompting.
		displayName := providerDisplayNames[provider]
		switch provider {
		case "hyper":
		case "copilot", "github", "github-copilot":
			provider = "copilot"
			displayName = providerDisplayNames[provider]
		case "openai", "chatgpt":
			provider = "openai"
			displayName = providerDisplayNames[provider]
		default:
			pc, ok := deviceAuthProvider(c, ws.ID, provider)
			if !ok {
				return fmt.Errorf("unknown platform: %s", provider)
			}
			displayName = cmp.Or(pc.Name, provider)
		}

		force, _ := cmd.Flags().GetBool("force")
		// Picking a platform from the list is an explicit choice already,
		// so only ask for confirmation when no choice was made.
		if !force && !chose {
			ok, err := logout.Confirm(fmt.Sprintf("Are you sure you want to log out of %s?", displayName))
			if err != nil {
				return err
			}
			if !ok {
				fmt.Println("Logout cancelled.")
				return nil
			}
		}

		switch provider {
		case "hyper":
			return logoutHyper(c, ws.ID)
		case "copilot":
			return logoutCopilot(c, ws.ID)
		case "openai":
			return logoutOpenAI(c, ws.ID)
		default:
			return logoutDevice(c, ws.ID, provider, displayName)
		}
	},
}

func logoutHyper(c *client.Client, wsID string) error {
	ctx := getLogoutContext()

	if err := cmp.Or(
		c.RemoveConfigField(ctx, wsID, config.ScopeGlobal, "providers.hyper.api_key"),
		c.RemoveConfigField(ctx, wsID, config.ScopeGlobal, "providers.hyper.oauth"),
	); err != nil {
		return err
	}

	fmt.Printf("Successfully logged out of %s.\n", providerDisplayNames["hyper"])
	return nil
}

func logoutCopilot(c *client.Client, wsID string) error {
	ctx := getLogoutContext()

	if err := cmp.Or(
		c.RemoveConfigField(ctx, wsID, config.ScopeGlobal, "providers.copilot.api_key"),
		c.RemoveConfigField(ctx, wsID, config.ScopeGlobal, "providers.copilot.oauth"),
	); err != nil {
		return err
	}

	fmt.Printf("Successfully logged out of %s.\n", providerDisplayNames["copilot"])
	return nil
}

func logoutOpenAI(c *client.Client, wsID string) error {
	ctx := getLogoutContext()

	// Logout clears every OpenAI credential: the ChatGPT token and its
	// model catalog, and the API key too. The API key mirrors the access
	// token when it came from the OAuth flow, and an explicit logout
	// should leave nothing behind either way.
	if err := cmp.Or(
		c.RemoveConfigField(ctx, wsID, config.ScopeGlobal, "providers.openai.oauth"),
		c.RemoveConfigField(ctx, wsID, config.ScopeGlobal, "providers.openai.chatgpt_models"),
		c.RemoveConfigField(ctx, wsID, config.ScopeGlobal, "providers.openai.api_key"),
	); err != nil {
		return err
	}

	fmt.Printf("Successfully logged out of %s.\n", providerDisplayNames["openai"])
	return nil
}

// pickLoggedInProvider returns the provider to log out of and whether the
// user explicitly picked it from a list of logged-in platforms.
func pickLoggedInProvider(c *client.Client, wsID string) (string, bool, error) {
	ctx := getLogoutContext()

	cfg, err := c.GetConfig(ctx, wsID)
	if err != nil {
		return "", false, fmt.Errorf("failed to get config: %w", err)
	}

	// Only OAuth-based providers support login/logout. Keep this list in
	// sync with the switch in RunE and the login command.
	var loggedIn []struct {
		id   string
		name string
	}
	for _, id := range []string{"hyper", "copilot", "openai"} {
		if p, ok := cfg.Providers.Get(id); ok && p.OAuthToken != nil {
			loggedIn = append(loggedIn, struct {
				id   string
				name string
			}{id: id, name: providerDisplayNames[id]})
		}
	}
	// Custom providers signed in through the OAuth device flow.
	for id, p := range cfg.Providers.Seq2() {
		if _, builtin := providerDisplayNames[id]; builtin || !p.UsesDeviceAuth() || p.OAuthToken == nil {
			continue
		}
		loggedIn = append(loggedIn, struct {
			id   string
			name string
		}{id: id, name: cmp.Or(p.Name, id)})
	}

	if len(loggedIn) == 0 {
		fmt.Println("You are not logged in to any platform.")
		return "", false, nil
	}

	if len(loggedIn) == 1 {
		return loggedIn[0].id, false, nil
	}

	names := make([]string, len(loggedIn))
	for i, p := range loggedIn {
		names[i] = p.name
	}
	choice, err := logout.Choose("Which platform do you want to log out of?", names)
	if err != nil {
		return "", false, err
	}
	if choice < 0 {
		fmt.Println("Logout cancelled.")
		return "", false, nil
	}

	return loggedIn[choice].id, true, nil
}

func init() {
	logoutCmd.Flags().BoolP("force", "f", false, "Skip logout confirmation prompt")
}

func getLogoutContext() context.Context {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	go func() {
		<-ctx.Done()
		cancel()
		os.Exit(1)
	}()
	return ctx
}

// deviceAuthProvider returns the custom provider with the given ID when it
// signs in through the OAuth device flow.
func deviceAuthProvider(c *client.Client, wsID, providerID string) (config.ProviderConfig, bool) {
	cfg, err := c.GetConfig(getLogoutContext(), wsID)
	if err != nil {
		return config.ProviderConfig{}, false
	}
	pc, ok := cfg.Providers.Get(providerID)
	if !ok || !pc.UsesDeviceAuth() {
		return config.ProviderConfig{}, false
	}
	return pc, true
}

func logoutDevice(c *client.Client, wsID, providerID, displayName string) error {
	ctx := getLogoutContext()

	// The API key mirrors the access token, so both go. Dropping the
	// token also drops the client registration saved with it; the next
	// login registers again.
	if err := cmp.Or(
		c.RemoveConfigField(ctx, wsID, config.ScopeGlobal, fmt.Sprintf("providers.%s.api_key", providerID)),
		c.RemoveConfigField(ctx, wsID, config.ScopeGlobal, fmt.Sprintf("providers.%s.oauth", providerID)),
	); err != nil {
		return err
	}

	fmt.Printf("Successfully logged out of %s.\n", displayName)
	return nil
}
