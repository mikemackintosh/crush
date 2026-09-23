package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/shellconfig"
	"github.com/spf13/cobra"
)

var hookCmd = &cobra.Command{
	Use:   "hook",
	Short: "Manage lifecycle hooks",
	Long: `Add, remove and list the shell commands that run on agent lifecycle events.

Events: ` + strings.Join(config.HookEvents, ", ") + `

Changes are written to the global Crush config so nothing has to be edited by
hand; pass --workspace to write to this project's .crush/crush.json instead.
"crush hook add" takes the same flags as the "hook add" line in a crushrc file,
and hooks follow the Claude Code hook contract: JSON on stdin, exit 2 to block
with stderr as the reason, JSON on stdout for decisions and context.`,
	Example: `
# Refuse force pushes before the bash tool runs them
crush hook add PreToolUse --matcher "^bash$" --command ./hooks/no-force-push.sh --name no-force-push

# Log every prompt in the background
crush hook add UserPromptSubmit --command "jq -r .prompt >> ~/.crush-prompts.log" --async true

# Keep the agent working until the tests pass
crush hook add Stop --command ./hooks/tests-must-pass.sh

crush hook list
crush hook remove Stop --name tests-must-pass
  `,
}

var hookAddCmd = &cobra.Command{
	Use:   "add <event> [flags]",
	Short: "Add a hook to an event",
	Long: `Add a shell command that runs when the event fires.

Flags (the same as the crushrc "hook add" builtin):
      --command string   shell command to run (required)
      --name string      name used for later removal and shown in the TUI
      --matcher string   regex tested against the event's subject
      --timeout int      timeout in seconds (default 30)
      --async bool       run in the background without waiting
      --workspace        write to this project's .crush/crush.json`,
	DisableFlagParsing: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		workspaceScope, args := splitWorkspaceFlag(args)
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
				return cmd.Help()
			}
			return errors.New("usage: crush hook add <event> --command CMD [flags]")
		}
		event, ok := canonicalHookEvent(args[0])
		if !ok {
			return fmt.Errorf("unknown hook event %q; known events: %s", args[0], strings.Join(config.HookEvents, ", "))
		}
		hook, err := shellconfig.HookFromArgs(args[1:])
		if err != nil {
			return err
		}

		c, ws, cleanup, err := connectToServer(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		ctx := getHookContext()
		cfg, err := c.GetConfig(ctx, ws.ID)
		if err != nil {
			return fmt.Errorf("failed to get config: %w", err)
		}
		// The event's list is replaced as a whole: the config store writes
		// JSON paths, and appending to an array through one is not a thing.
		// A hook with the same name as an existing one replaces it.
		list := hookMaps(cfg.Hooks[event])
		name, _ := hook["name"].(string)
		replaced := false
		for i, existing := range list {
			if name != "" && existing["name"] == name {
				list[i] = hook
				replaced = true
			}
		}
		if !replaced {
			list = append(list, hook)
		}
		scope := scopeFor(workspaceScope)
		if err := c.SetConfigField(ctx, ws.ID, scope, "hooks."+event, list); err != nil {
			return err
		}
		fmt.Printf("Hook added to %s in the %s config (%d hook(s) on the event).\n", event, scope, len(list))
		return nil
	},
}

var hookRemoveCmd = &cobra.Command{
	Use:     "remove <event>",
	Aliases: []string{"rm"},
	Short:   "Remove a named hook, or every hook on an event",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		event, ok := canonicalHookEvent(args[0])
		if !ok {
			return fmt.Errorf("unknown hook event %q; known events: %s", args[0], strings.Join(config.HookEvents, ", "))
		}
		name, _ := cmd.Flags().GetString("name")
		workspaceScope, _ := cmd.Flags().GetBool("workspace")

		c, ws, cleanup, err := connectToServer(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		ctx := getHookContext()
		cfg, err := c.GetConfig(ctx, ws.ID)
		if err != nil {
			return fmt.Errorf("failed to get config: %w", err)
		}
		existing := cfg.Hooks[event]
		if len(existing) == 0 {
			return fmt.Errorf("no hooks are configured on %s", event)
		}
		scope := scopeFor(workspaceScope)
		if name == "" {
			if err := c.RemoveConfigField(ctx, ws.ID, scope, "hooks."+event); err != nil {
				return err
			}
			fmt.Printf("Removed every hook on %s from the %s config.\n", event, scope)
			return nil
		}
		var kept []map[string]any
		for _, h := range hookMaps(existing) {
			if h["name"] != name {
				kept = append(kept, h)
			}
		}
		if len(kept) == len(existing) {
			return fmt.Errorf("no hook named %q on %s", name, event)
		}
		if len(kept) == 0 {
			if err := c.RemoveConfigField(ctx, ws.ID, scope, "hooks."+event); err != nil {
				return err
			}
		} else if err := c.SetConfigField(ctx, ws.ID, scope, "hooks."+event, kept); err != nil {
			return err
		}
		fmt.Printf("Removed hook %q from %s in the %s config.\n", name, event, scope)
		return nil
	},
}

var hookListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List configured hooks by event",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, ws, cleanup, err := connectToServer(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		cfg, err := c.GetConfig(getHookContext(), ws.ID)
		if err != nil {
			return fmt.Errorf("failed to get config: %w", err)
		}
		total := 0
		for _, event := range config.HookEvents {
			list := cfg.Hooks[event]
			if len(list) == 0 {
				continue
			}
			fmt.Println(event)
			for _, h := range list {
				fmt.Println("  " + describeHook(h))
				total++
			}
		}
		if total == 0 {
			fmt.Println("No hooks are configured. Add one with: crush hook add <event> --command CMD")
		}
		return nil
	},
}

// describeHook renders one hook as a single line.
func describeHook(h config.HookConfig) string {
	parts := []string{h.DisplayName()}
	if h.Matcher != "" {
		parts = append(parts, "matcher="+h.Matcher)
	}
	if h.Timeout > 0 {
		parts = append(parts, fmt.Sprintf("timeout=%ds", h.Timeout))
	}
	if h.Async {
		parts = append(parts, "async")
	}
	if h.Name != "" && h.Name != h.Command {
		parts = append(parts, "command="+h.Command)
	}
	return strings.Join(parts, "  ")
}

// canonicalHookEvent maps a user-typed event name to its canonical spelling,
// accepting case and separator differences the config loader also accepts.
func canonicalHookEvent(name string) (string, bool) {
	key := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(name))
	for _, ev := range config.HookEvents {
		if strings.ToLower(ev) == key {
			return ev, true
		}
	}
	return "", false
}

// hookMaps converts loaded hook configs back to the JSON shape the config
// store writes, dropping zero values so the file stays tidy.
func hookMaps(list []config.HookConfig) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, h := range list {
		m := map[string]any{"command": h.Command}
		if h.Name != "" {
			m["name"] = h.Name
		}
		if h.Matcher != "" {
			m["matcher"] = h.Matcher
		}
		if h.Timeout > 0 {
			m["timeout"] = h.Timeout
		}
		if h.Async {
			m["async"] = true
		}
		out = append(out, m)
	}
	return out
}

func getHookContext() context.Context {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	go func() {
		<-ctx.Done()
		cancel()
	}()
	return ctx
}

func init() {
	hookRemoveCmd.Flags().String("name", "", "Remove only the hook with this name")
	hookRemoveCmd.Flags().Bool("workspace", false, "Remove from this project's .crush/crush.json instead of the global config")
	hookCmd.AddCommand(hookAddCmd, hookRemoveCmd, hookListCmd)
}
