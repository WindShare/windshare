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
)

func TestNativeSetupPreservesSavedRelaySelectionAndDisabledDefaults(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "explicit"}[enabled], func(t *testing.T) {
			t.Setenv(relayconfig.PathEnvironment, filepath.Join(t.TempDir(), "settings.json"))
			store := relayconfig.FileStore{}
			initial := relayconfig.Config{
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
			if err := nativeSetup(t.Context(), SetupInput{RelayURL: server.URL}, nil); err != nil {
				t.Fatal(err)
			}
			saved, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(saved.ActiveRelays, initial.ActiveRelays) || saved.FrontURL != initial.FrontURL {
				t.Fatal("setup replaced saved selection or frontend")
			}
			if len(saved.Profiles) != 2 || saved.Profiles[1].PublicURL != server.URL {
				t.Fatal("setup did not retain the newly configured relay")
			}
		})
	}
}
