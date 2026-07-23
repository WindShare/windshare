package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/internal/testnetwork"
	"github.com/windshare/windshare/relay/httpapi"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2endpoint"
	"github.com/windshare/windshare/relay/signaling/v2route"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestParseInterleavedV2Flags(t *testing.T) {
	app := testApp("")
	flags := app.newFlagSet("share")
	relay := flags.String("relay", "", "")
	positionals, err := parseInterleaved(flags, []string{"first", "--relay", "ws://relay.example", "second"})
	if err != nil || *relay != "ws://relay.example" || strings.Join(positionals, ",") != "first,second" {
		t.Fatalf("parse result = %v %q %v", positionals, *relay, err)
	}
}

func TestGetRequestRejectsRetiredSuite(t *testing.T) {
	shareID := base64.RawURLEncoding.EncodeToString(
		bytes.Repeat([]byte{1}, link.SenderAuthenticatedShareIDBytes),
	)
	retiredKey := base64.RawURLEncoding.EncodeToString(
		append([]byte{0x01}, bytes.Repeat([]byte{2}, link.ReadSecretBytes)...),
	)
	encoded := "http://localhost:5173/" + shareID + "#" + retiredKey
	app := testApp("")
	if _, code := app.parseGetRequest([]string{encoded}); code != ExitUsage {
		t.Fatalf("retired suite exit code = %d", code)
	}
}

func TestRelayRegistrationIdentityRejectsWrongWidths(t *testing.T) {
	if _, _, _, err := relayRegistrationIdentity(liveshare.RegistrationMaterial{}); err == nil {
		t.Fatal("empty relay identity was accepted")
	}
}

func TestTransferResultDriftClassification(t *testing.T) {
	if !transferResultDrifted(transfer.JobResult{AbortCause: content.ErrRevisionStale}) {
		t.Fatal("revision drift was not classified")
	}
	if transferResultDrifted(transfer.JobResult{AbortCause: errors.New("network")}) {
		t.Fatal("network failure was classified as drift")
	}
}

func TestShareCancellationDurablyStopsRelayRoute(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	store := &memoryStopStore{}
	server := httptest.NewUnstartedServer(nil)
	endpointIdentity, err := v2.NormalizeRelayEndpoint("http://" + server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := v2route.New(context.Background(), v2route.Config{
		MaxRoutes: 8, MaxSessions: 8, MaxSessionsPerShare: 4, Random: rand.Reader, Tombstones: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	challenges, err := v2.NewChallengeLedger(v2.ChallengeLedgerConfig{Capacity: 16, Random: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := v2endpoint.New(v2endpoint.Config{
		Registry: registry, Challenges: challenges, RelayIdentity: endpointIdentity.Identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = httpapi.NewV2Handler(httpapi.V2Config{
		Server: endpoint, AllowLocalhost: true,
		AdmitConnection: func(string) (func(), bool) { return func() {}, true },
	})
	server.Start()
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = endpoint.Shutdown(shutdown)
		cancel()
		server.Close()
	})

	file := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(file, []byte("stop contract"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout := &lockedTestBuffer{}
	stderr := &lockedTestBuffer{}
	app := &App{Stdout: stdout, Stderr: stderr, Stdin: strings.NewReader("")}
	shareContext, cancelShare := context.WithCancel(context.Background())
	result := make(chan int, 1)
	go func() {
		result <- app.Run(shareContext, []string{"share", file, "--relay", server.URL})
	}()
	linkLine := waitTestLine(t, stdout, "Link: ")
	capability, err := link.Parse(strings.TrimPrefix(linkLine, "Link: "))
	if err != nil {
		t.Fatal(err)
	}
	shareIDBytes, _ := base64.RawURLEncoding.Strict().DecodeString(capability.ShareID)
	shareID, _ := v2.ShareIDFromBytes(shareIDBytes)
	joined, err := relayv2.DialReceiver(context.Background(), relayv2.ReceiverConfig{
		RelayBaseURL: server.URL, ShareID: shareID,
	})
	if err != nil {
		t.Fatalf("link was printed before relay readiness: %v", err)
	}
	_ = joined.Close()
	for index, arguments := range [][]string{
		{"get", strings.TrimPrefix(linkLine, "Link: "), "-o", t.TempDir()},
		{"get", strings.TrimPrefix(linkLine, "Link: "), "-o", t.TempDir(), "--only", filepath.Base(file)},
	} {
		getOutput := &lockedTestBuffer{}
		getErrors := &lockedTestBuffer{}
		getApp := &App{Stdout: getOutput, Stderr: getErrors, Stdin: strings.NewReader("")}
		if code := getApp.Run(context.Background(), arguments); code != ExitOK {
			t.Fatalf("get %d exit=%d stderr=%q", index, code, getErrors.String())
		}
	}
	cancelShare()
	select {
	case code := <-result:
		if code != ExitOK {
			t.Fatalf("share cancellation exit=%d stderr=%q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("share did not complete explicit stop")
	}
	if store.Count() != 1 {
		t.Fatalf("durable STOP writes = %d", store.Count())
	}
	_, err = relayv2.DialReceiver(context.Background(), relayv2.ReceiverConfig{
		RelayBaseURL: server.URL, ShareID: shareID,
	})
	var relayError *relayv2.RelayError
	if !errors.As(err, &relayError) || relayError.Code != v2.ErrorStopped {
		t.Fatalf("join after explicit stop = %v", err)
	}
}

type lockedTestBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *lockedTestBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(value)
}

func (buffer *lockedTestBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

func waitTestLine(t *testing.T, output *lockedTestBuffer, prefix string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for line := range strings.SplitSeq(output.String(), "\n") {
			if strings.HasPrefix(line, prefix) {
				return line
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in %q", prefix, output.String())
	return ""
}

type memoryStopStore struct {
	mu     sync.Mutex
	values []v2route.Tombstone
}

func (store *memoryStopStore) Load(context.Context) ([]v2route.Tombstone, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]v2route.Tombstone(nil), store.values...), nil
}

func (store *memoryStopStore) Commit(
	_ context.Context,
	value v2route.Tombstone,
) (v2route.CommitOutcome, error) {
	store.mu.Lock()
	store.values = append(store.values, value)
	store.mu.Unlock()
	return v2route.CommitCommitted, nil
}

func (store *memoryStopStore) Count() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.values)
}

func testApp(stdin string) *App {
	return &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Stdin: strings.NewReader(stdin)}
}

func TestRegistrationMaterialUsesEd25519Width(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(privateKey) != ed25519.PrivateKeySize || catalog.IdentityBytes != v2.ShareInstanceBytes {
		t.Fatal("suite-02 identity widths diverged")
	}
}
