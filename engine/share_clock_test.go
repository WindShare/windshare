package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/engine/networkdirectory"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/osfs"
)

type doubleSpeedClock struct{ origin, started time.Time }

func (c doubleSpeedClock) Now() time.Time { return c.origin.Add(2 * time.Since(c.started)) }
func (c doubleSpeedClock) Wait(ctx context.Context, delay time.Duration, wake <-chan struct{}) error {
	return (senderrelay.SystemClock{}).Wait(ctx, (delay+1)/2, wake)
}

type unavailableTransport struct{ calls atomic.Int32 }

func (transport *unavailableTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	transport.calls.Add(1)
	return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {"60"}}, Body: http.NoBody, Request: r}, nil
}

func TestSharesHonorNodeDeadlineOnEngineClock(t *testing.T) {
	var fixture struct {
		Now     time.Time
		Roots   []struct{ ID, Key string }
		Entries []struct{ Envelope json.RawMessage }
	}
	data, err := os.ReadFile("../testdata/network/directory.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	key, err := base64.RawURLEncoding.DecodeString(fixture.Roots[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	source := liveshare.FileSourceFactoryFunc(func(ctx context.Context, scope liveshare.FileSourceContext) (liveshare.FileSource, error) {
		return osfs.NewSelectedFileSource(ctx, osfs.SelectedCatalogSourceConfig{Paths: []string{root}, SyntheticRoot: scope.SyntheticRoot, Identities: osfs.CatalogIdentitySourceFunc(scope.NewIdentity)})
	})
	synctest.Test(t, func(t *testing.T) {
		transport := &unavailableTransport{}
		previous := http.DefaultClient.Transport
		http.DefaultClient.Transport = transport
		t.Cleanup(func() { http.DefaultClient.Transport = previous })
		var fetches atomic.Int32
		config := networkdirectory.Config{Network: "test", Roots: []nodedirectory.Root{{ID: fixture.Roots[0].ID, Key: key}},
			Embedded: fixture.Entries[0].Envelope, Mirrors: []string{"https://mirror.example/network"},
			Fetch: func(context.Context, string) ([]byte, error) {
				fetches.Add(1)
				return fixture.Entries[0].Envelope, nil
			},
		}
		app := newTestEngine(t, Config{Clock: doubleSpeedClock{origin: fixture.Now, started: time.Now()}, Network: &networkconfig.Config{Directory: config},
			Share: ShareDependencies{Peers: func() (SharePeers, error) { return &facadePeers{}, nil }, PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() }},
		})
		start := func() *ShareTask {
			t.Helper()
			task, err := app.StartShare(t.Context(), ShareRequest{Source: source, RelayIntent: relayprofile.Plan(nil)})
			if err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			return task
		}
		first := start()
		node := first.selection.Plan.Network[0]
		deadline := fixture.Now.Add(time.Minute)
		if got := app.network.ServerRetryAt(node.Identity); !got.Equal(deadline) || transport.calls.Load() != 1 || fetches.Load() != 1 {
			t.Fatalf("first share deadline=%v dials=%d refreshes=%d", got, transport.calls.Load(), fetches.Load())
		}
		first.StopShare()
		if _, err := first.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}

		// The next plan can refresh the directory after 30 engine seconds. Only
		// the absolute server deadline continues to bind the second registration.
		time.Sleep(15 * time.Second)
		second := start()
		defer second.StopShare()
		next := second.selection.Plan.Network[0]
		if !second.selection.Decision.CooldownFallback || !next.ServerRetryAt.Equal(deadline) || !next.CooldownUntil.IsZero() || fetches.Load() != 2 {
			t.Fatalf("second plan=%+v refreshes=%d", second.selection.Decision, fetches.Load())
		}
		time.Sleep(15*time.Second - time.Nanosecond)
		synctest.Wait()
		if transport.calls.Load() != 1 {
			t.Fatal("admission bypassed the server deadline")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if transport.calls.Load() != 2 {
			t.Fatal("admission did not wake at the engine deadline")
		}
	})
}
