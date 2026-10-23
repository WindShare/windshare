package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/engine/internal/turnruntime"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/engine/turn"
)

const trialLifetimeSeconds = 300

type trialTURN struct {
	runtime   *turnruntime.Runtime
	selection *turnruntime.Selection
	release   func()
}

func trialIssuer() (*trialTURN, error) {
	settings, relays, err := trialSettings()
	if err != nil {
		return nil, err
	}
	runtime, err := turnruntime.New(settings, turn.Dependencies{})
	if err != nil {
		return nil, err
	}
	selection, release := runtime.ForShare(context.Background(), relays)
	return &trialTURN{runtime: runtime, selection: selection, release: release}, nil
}

func (source *trialTURN) Close() error {
	source.release()
	return source.runtime.Close()
}

func trialSettings() (turn.Settings, []relayprofile.Candidate, error) {
	if path := os.Getenv("WINDSHARE_TURN_TRIAL_CONFIGURATION"); path != "" {
		config, err := (relayconfig.FileStore{Path: path}).Load()
		if err != nil {
			return turn.Settings{}, nil, err
		}
		if len(config.Profiles) != 1 {
			return turn.Settings{}, nil, errors.New("trial requires one enrolled deployment")
		}
		settings := config.TURNSettings()
		settings.Manual = turn.ManualSettings{}
		return settings, config.Profiles, nil
	}
	data, err := os.ReadFile(os.Getenv("WINDSHARE_TURN_TRIAL_KEY"))
	if err != nil {
		return turn.Settings{}, nil, err
	}
	var key struct {
		ID     string `json:"uid"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(data, &key); err != nil {
		return turn.Settings{}, nil, err
	}
	credentials, err := json.Marshal(turn.CloudflareCredentials{KeyID: key.ID, APIToken: key.Secret, LifetimeSeconds: trialLifetimeSeconds})
	if err != nil {
		return turn.Settings{}, nil, err
	}
	settings := turn.Settings{Manual: turn.ManualSettings{
		Accounts: []turn.AccountSettings{{ID: "trial", Provider: "cloudflare", Issuer: turn.CloudflareIssuer, Credentials: credentials}},
		Profiles: []turn.ProfileSettings{{ID: "trial", Account: "trial"}},
	}}
	return settings, nil, nil
}

func (p *probe) issue(ctx context.Context, id, previous byte) (*turnpath.Lease, error) {
	subject := turnpath.Subject{SessionID: [16]byte{1}, PathID: [16]byte{id}}
	if previous != 0 {
		p.mu.Lock()
		old := p.issued[previous]
		p.mu.Unlock()
		if old != nil && old.Renewable() {
			return old.Renew(ctx, subject)
		}
	}
	for {
		inventory := p.source.selection.Inventory()
		if len(inventory.Profiles) > 0 {
			return p.source.selection.Acquire(ctx, inventory.Profiles[0].Identity.Profile, subject)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-inventory.Changed:
		}
	}
}
