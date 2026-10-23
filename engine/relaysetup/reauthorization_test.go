package relaysetup

import (
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
)

func TestReauthorizationCommitsCredentialAndDetectsConcurrentCredentialImport(t *testing.T) {
	for _, operation := range []Operation{AddProfile, PrivateOnboarding} {
		for _, scenario := range []string{"replace-credential", "concurrent-credential-import"} {
			t.Run(string(operation)+"/"+scenario, func(t *testing.T) {
				const address = "https://private.example"
				const priorKey = "prior-key"
				const importedKey = "imported-key"
				now := time.Unix(100, 0)
				settings := relayconfig.FileStore{Path: filepath.Join(t.TempDir(), "config.json")}
				config := relayconfig.Config{
					FrontURL:     address,
					Profiles:     []relayprofile.Candidate{{PublicURL: address, AccessKey: priorKey}},
					ActiveRelays: []string{address},
				}
				if err := settings.Save(config); err != nil {
					t.Fatal(err)
				}
				store := FileStore{Settings: settings}
				pending := pendingCredential(now).Pending
				pending.Operation = operation
				pending.SettingsRevision = config.SettingsRevision()
				if _, err := store.Update(t.Context(), func(current *Snapshot) error {
					current.Pending = pending
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				confirmations := 0
				service := New(Dependencies{Store: store, Now: func() time.Time { return now },
					HTTP: fakeHTTP(func(req *http.Request) (*http.Response, error) {
						if req.URL.Path != "/api/enrollments/installed" {
							t.Fatalf("unexpected request: %s", req.URL)
						}
						confirmations++
						if scenario == "concurrent-credential-import" {
							imported := config
							imported.Profiles = slices.Clone(config.Profiles)
							imported.Profiles[0].AccessKey = importedKey
							if err := (relayconfig.FileStore{Path: settings.Path}).Save(imported); err != nil {
								t.Fatal(err)
							}
						}
						return response(http.StatusOK, map[string]bool{"ok": true})
					})})
				result, runErr := service.Run(t.Context(), Request{URL: address, Operation: operation}, nil)
				current, err := (FileStore{Settings: settings}).Load()
				if err != nil {
					t.Fatal(err)
				}
				if confirmations != 1 || len(current.Profiles) != 1 || current.FrontURL != address ||
					!slices.Equal(current.ActiveRelays, config.ActiveRelays) {
					t.Fatal("reauthorization changed relay selection or skipped confirmation")
				}
				if scenario == "concurrent-credential-import" {
					if !errors.Is(runErr, ErrSettingsChanged) || current.Profiles[0].AccessKey != importedKey ||
						current.Pending == nil || current.Pending.credential().AccessKey != pending.credential().AccessKey {
						t.Fatalf("concurrent credential import lost its authority or cancellation checkpoint: %v", runErr)
					}
					return
				}
				if runErr != nil || result.CredentialID != pending.credential().ID || current.Pending != nil ||
					current.Profiles[0].AccessKey != pending.credential().AccessKey {
					t.Fatalf("successful reauthorization did not persist the approved credential: %v", runErr)
				}
			})
		}
	}
}
