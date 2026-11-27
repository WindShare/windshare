package cli

import (
	"context"

	"github.com/windshare/windshare/cmd/wind/internal/command"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relaysetup"
)

func (app *App) defaultFrontend() string {
	if network := app.engineConfig.Network; network != nil {
		return network.LinkHost
	}
	return "default receiver website"
}

func relayEnabledLabel(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func (app *App) parseRelayPreferenceArgs(args []string) (string, string, int, bool) {
	action := args[0]
	flags := command.NewFlagSet("relay " + action)
	if action == "list" {
		outcome := command.ParseNoPositionals(flags, args[1:])
		if outcome == command.ParseHelp {
			app.writeCompleteLine("%s", relayUsage)
			return "", "", ExitOK, false
		}
		if outcome != command.ParseReady {
			app.writeCompleteLine("relay list: %s", outcome.Diagnostic())
			return "", "", outcome.ExitCode().Int(), false
		}
		return action, "", ExitOK, true
	}
	positionals, outcome := command.ParseInterleaved(flags, args[1:])
	if outcome == command.ParseHelp {
		app.writeCompleteLine("%s", relayUsage)
		return "", "", ExitOK, false
	}
	if outcome != command.ParseReady {
		app.writeCompleteLine("relay %s: %s", action, outcome.Diagnostic())
		return "", "", outcome.ExitCode().Int(), false
	}
	if len(positionals) != 1 || positionals[0] == "" {
		app.writeCompleteLine("%s", relayUsage)
		return "", "", ExitUsage, false
	}
	return action, positionals[0], ExitOK, true
}

func (app *App) relayPreferences(ctx context.Context, args []string) int {
	action, value, code, ready := app.parseRelayPreferenceArgs(args)
	if !ready {
		return code
	}
	store := app.engineConfig.RelaySetup.Store
	if store == nil {
		return app.writeRelaySetupFailure(relayconfig.ErrConfig)
	}
	var saved relaysetup.Snapshot
	var err error
	if action == "list" {
		saved, err = store.Load()
	} else {
		change := relayconfig.PreferenceEdit{Action: action, Value: value}
		switch {
		case change.Action == "community":
			change.Action = "community_nodes"
		case change.Action == "frontend" && change.Value == "default":
			change.Value = ""
		}
		saved, err = store.Update(ctx, func(snapshot *relaysetup.Snapshot) error { return change.Apply(&snapshot.Config) })
	}
	if err != nil {
		return app.writeRelaySetupFailure(err)
	}
	view := saved.Preferences(app.defaultFrontend())
	app.writeCompleteLine("Relay mode: %s. Share website: %s", view.Mode, view.Frontend)
	switch {
	case view.Mode == relayconfig.Automatic:
		app.writeCompleteLine("Community nodes: %s", relayEnabledLabel(view.CommunityNodes))
	case action == "community":
		app.writeCompleteLine("Community nodes: %s (applies in automatic mode)", relayEnabledLabel(view.CommunityNodes))
	}
	for _, relay := range view.Relays {
		app.writeCompleteLine("%s (%s)", relay.URL, relayEnabledLabel(relay.Enabled))
	}
	if view.AtCapacity && view.Mode == relayconfig.Automatic {
		app.writeCompleteLine("All eight relay slots are selected. Disable a relay to make room for the built-in network.")
	}
	return ExitOK
}
