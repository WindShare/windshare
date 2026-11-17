package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
	"github.com/windshare/windshare/engine"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestShareCancellationBeforePublicationSettlesWithoutRedialing(t *testing.T) {
	for _, rejected := range []bool{true, false} {
		name := "hung upgrade"
		if rejected {
			name = "rejected upgrade"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), cliShareCancellationTimeout)
			defer cancel()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if rejected {
					http.NotFound(w, r)
					w.(http.Flusher).Flush()
				}
				cancel()
				if !rejected {
					<-r.Context().Done()
				}
			}))
			defer server.Close()
			file := filepath.Join(t.TempDir(), "file.txt")
			if err := os.WriteFile(file, []byte("unpublished share"), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			trace := newRecordingUserTrace()
			app := &App{
				Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader(""),
				engineConfig: engine.Config{Share: engine.ShareDependencies{
					PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
				}},
				openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
					return trace, nil
				},
			}
			code := app.Run(ctx, []string{"share", file, "--relay", server.URL, "--front-url", "https://front.example", "--trace", filepath.Join(t.TempDir(), "share.ndjson")})
			if code != ExitOK || stdout.Len() != 0 || requests.Load() != 1 {
				t.Fatalf("exit=%d stdout=%q requests=%d stderr=%q", code, stdout.String(), requests.Load(), stderr.String())
			}
			trace.mu.Lock()
			defer trace.mu.Unlock()
			terminal := 0
			for _, event := range trace.events {
				switch event := event.(type) {
				case clievent.SharingStopped:
					terminal++
					if event.Result().Publication() != clievent.ShareUnpublished || !event.Result().StoppedCleanly() {
						t.Fatalf("unpublished stop result = %+v", event.Result())
					}
				case appobs.Ready, clievent.CommandFailed:
					t.Fatalf("unpublished cancellation emitted %T", event)
				}
			}
			if terminal != 1 {
				t.Fatalf("terminal results = %d, want one unpublished stop", terminal)
			}
		})
	}
}
