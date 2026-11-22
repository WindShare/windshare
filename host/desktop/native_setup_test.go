package desktop

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"

	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/engine/relaysetup"
)

func TestNativeSetupEnablesNewRelaysWhilePreservingSavedChoices(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "explicit"}[enabled], func(t *testing.T) {
			t.Setenv(relayconfig.PathEnvironment, filepath.Join(t.TempDir(), "settings.json"))
			store := relayconfig.FileStore{}
			initial := relayconfig.Config{ConfiguredOnly: true,
				Profiles: []relayprofile.Candidate{{PublicURL: "https://saved.example"}},
				FrontURL: "https://saved-front.example",
			}
			if enabled {
				initial.ActiveRelays = []string{"https://saved.example"}
			}
			if err := store.Save(initial); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/.well-known/windshare" {
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"version": 1, "access": "open", "relay_url": "http://" + r.Host,
					"frontend_url": "https://discovered-front.example",
				})
			}))
			defer server.Close()
			setup := newTestNativeSetup(t, store)
			if _, err := setup.run(t.Context(), SetupInput{RelayURL: server.URL, RelayOperation: relaysetup.AddProfile}, nil); err != nil {
				t.Fatal(err)
			}
			saved, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			expectedActive := append(slices.Clone(initial.ActiveRelays), server.URL)
			if !slices.Equal(saved.ActiveRelays, expectedActive) || saved.FrontURL != initial.FrontURL {
				t.Fatalf("selection = %v (want %v), front = %q", saved.ActiveRelays, expectedActive, saved.FrontURL)
			}
			if len(saved.Profiles) != 2 || saved.Profiles[1].PublicURL != server.URL {
				t.Fatal("setup did not retain the newly configured relay")
			}
			// Disabling the added relay and running setup again keeps it disabled.
			if err := saved.SetEnabled(server.URL, false); err != nil {
				t.Fatal(err)
			}
			if err := store.Save(saved); err != nil {
				t.Fatal(err)
			}
			if _, err := setup.run(t.Context(), SetupInput{RelayURL: server.URL, RelayOperation: relaysetup.AddProfile}, nil); err != nil {
				t.Fatal(err)
			}
			remerged, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(remerged.ActiveRelays, initial.ActiveRelays) || remerged.FrontURL != initial.FrontURL {
				t.Fatalf("rediscovery re-enabled disabled relay or changed frontend: %+v", remerged)
			}
		})
	}
}
