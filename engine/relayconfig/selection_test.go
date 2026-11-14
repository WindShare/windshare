package relayconfig

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/engine/relayprofile"
)

func TestShareIntentRejectsEmptyConfiguredSelectionAcrossCallers(t *testing.T) {
	const relay = "https://relay.example"
	for _, test := range []struct {
		name            string
		config          Config
		override        []string
		wantMode        relayprofile.IntentMode
		wantCandidates  int
		wantConfigError bool
	}{
		{name: "default planning", wantMode: relayprofile.Planned},
		{name: "configured only without candidates", config: Config{ConfiguredOnly: true}, wantConfigError: true},
		{name: "inactive profiles are not candidates", config: Config{ConfiguredOnly: true,
			Profiles: []relayprofile.Candidate{{PublicURL: relay}}}, wantConfigError: true},
		{name: "active configured relay", config: Config{ConfiguredOnly: true, ActiveRelays: []string{relay}},
			wantMode: relayprofile.ConfiguredOnly, wantCandidates: 1},
		{name: "explicit relay overrides empty configuration", config: Config{ConfiguredOnly: true}, override: []string{relay},
			wantMode: relayprofile.Explicit, wantCandidates: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			intent, err := ShareIntent(test.config, test.override)
			if test.wantConfigError {
				if !errors.Is(err, ErrConfig) || !errors.Is(err, relayprofile.ErrCandidatesRequired) {
					t.Fatalf("missing configuration lost its domain cause: %v", err)
				}
				return
			}
			if err != nil || intent.Mode() != test.wantMode || len(intent.Candidates()) != test.wantCandidates {
				t.Fatalf("intent=%+v err=%v", intent, err)
			}
		})
	}
}
