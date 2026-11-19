package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/windshare/windshare/engine"
)

func TestShareVerboseUpgradeFailuresAreRelayUnavailable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"forbidden upgrade", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusForbidden)
		}},
		{"close after handshake", func(w http.ResponseWriter, r *http.Request) {
			connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			_ = connection.Close(websocket.StatusNormalClosure, "bye")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			server := httptest.NewServer(test.handler)
			t.Cleanup(server.Close)
			stdout, stderr := &lockedTestBuffer{}, &lockedTestBuffer{}
			app := &App{
				Stdout: stdout, Stderr: stderr, Stdin: strings.NewReader(""),
				engineConfig: engine.Config{Share: engine.ShareDependencies{
					PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
				}},
			}
			done := make(chan int, 1)
			go func() {
				done <- app.Run(ctx, []string{"share", file, "--relay", server.URL, "--front-url", "https://front.example", "-v"})
			}()
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if strings.Contains(stderr.String(), "Relay recovery attempt 1 failed") {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
			code := <-done
			text := stderr.String()
			if !strings.Contains(text, "Relay recovery attempt 1 failed") || !strings.Contains(text, "The relay is unavailable.") ||
				strings.Contains(text, "unexpected") || strings.Contains(text, "bye") || strings.Contains(text, "close frame") {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), text)
			}
		})
	}
}
