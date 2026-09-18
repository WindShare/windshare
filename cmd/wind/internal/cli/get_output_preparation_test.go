package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/transport/relayv2"
)

type bindingOnlyGetOutputAuthority struct {
	engine.OutputAuthority
	events *[]string
}

func (authority *bindingOnlyGetOutputAuthority) BindDestination(context.Context) (engine.OutputMode, error) {
	*authority.events = append(*authority.events, "bind")
	return engine.OutputResumable, nil
}

func (authority *bindingOnlyGetOutputAuthority) Close() error {
	*authority.events = append(*authority.events, "close")
	return nil
}

func TestGetDefaultsToCurrentDirectoryAndBindsItBeforeSessionWork(t *testing.T) {
	capability := newGetOutputPreparationCapability(t, "wss://relay.example")
	encoded, err := capability.URL("https://app.example")
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	app := &App{Stderr: &stderr}
	request, parsed := app.parseGetRequest([]string{encoded})
	if parsed != requestParseReady || request.outDir != "." {
		t.Fatalf("request=%+v parsed=%v", request, parsed)
	}
	var events []string
	app.getOutputFactory = engine.OutputFactoryFunc(func(config engine.OutputConfig) (engine.OutputAuthority, error) {
		if config.Tracer == nil {
			t.Fatal("missing output observations")
		}
		events = append(events, "construct")
		return &bindingOnlyGetOutputAuthority{events: &events}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app.receiverDial = func(context.Context, relayv2.ReceiverConfig) (*relayv2.ReceiverConnection, error) {
		events = append(events, "dial")
		cancel()
		return nil, context.Canceled
	}
	if code := app.Run(ctx, []string{"get", encoded}); code != ExitNetwork {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if strings.Join(events, ",") != "construct,bind,dial,close" {
		t.Fatalf("authority order=%v", events)
	}
}

func TestGetExistingTracePrecedesOutputMutation(t *testing.T) {
	capability := newGetOutputPreparationCapability(t, "wss://relay.example")
	encoded, err := capability.URL("https://app.example")
	if err != nil {
		t.Fatal(err)
	}
	stderr := &bytes.Buffer{}
	outputCalls := 0
	app := &App{
		Stderr: stderr,
		openUserTrace: func(
			runtrace.Target,
			clievent.Command,
			runtrace.Config,
			runtrace.Dependencies,
		) (userTraceRecorder, error) {
			return nil, runtrace.ErrTraceExists
		},
		getOutputFactory: engine.OutputFactoryFunc(func(engine.OutputConfig) (engine.OutputAuthority, error) {
			outputCalls++
			return nil, errors.New("output construction must not run")
		}),
	}
	if code := app.Run(t.Context(), []string{"get", "--trace", filepath.Join(t.TempDir(), "get.ndjson"), encoded}); code != ExitFailure {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if outputCalls != 0 {
		t.Fatalf("trace open failure allowed %d output mutation(s)", outputCalls)
	}
	for _, want := range []string{"already exists", "prior evidence was preserved", "command/output state was untouched", "--trace-dir"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("trace open diagnostic=%q missing %q", stderr.String(), want)
		}
	}
}

func TestGetNativeExistingTraceReturnsTypedFailureWithoutPanic(t *testing.T) {
	capability := newGetOutputPreparationCapability(t, "wss://relay.example")
	encoded, err := capability.URL("https://app.example")
	if err != nil {
		t.Fatal(err)
	}
	tracePath := filepath.Join(t.TempDir(), "existing.ndjson")
	if err := os.WriteFile(tracePath, []byte("retained\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	app := &App{Stderr: &stderr}
	if code := app.Run(t.Context(), []string{"get", encoded, "--trace", tracePath}); code != ExitFailure {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	for _, want := range []string{"already exists", "prior evidence was preserved", "command/output state was untouched", "--trace-dir"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("trace open diagnostic=%q missing %q", stderr.String(), want)
		}
	}
	retained, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(retained) != "retained\n" {
		t.Fatalf("existing trace was changed: %q", retained)
	}
}

func TestGetTraceDirectoryOwnsAndReportsRunFileBeforeOutputConstruction(t *testing.T) {
	capability := newGetOutputPreparationCapability(t, "wss://relay.example")
	encoded, err := capability.URL("https://app.example")
	if err != nil {
		t.Fatal(err)
	}
	traceDirectory := filepath.Join(t.TempDir(), "new", "nested", "traces")
	var stdout, stderr bytes.Buffer
	outputCalls := 0
	app := &App{
		Stdout: &stdout, Stderr: &stderr,
		getOutputFactory: engine.OutputFactoryFunc(func(engine.OutputConfig) (engine.OutputAuthority, error) {
			outputCalls++
			entries, readErr := os.ReadDir(traceDirectory)
			if readErr != nil || len(entries) != outputCalls {
				t.Fatalf("trace entries before output construction = %v, %v", entries, readErr)
			}
			for _, entry := range entries {
				path := filepath.Join(traceDirectory, entry.Name())
				if strings.Count(stderr.String(), terminalcanvas.EscapeText(path)) != 1 {
					t.Fatalf("generated trace path was not reported exactly once before output construction: %q", stderr.String())
				}
			}
			return nil, errors.New("output construction canary")
		}),
	}
	for run := range 2 {
		if code := app.Run(t.Context(), []string{"get", "--trace-dir", traceDirectory, encoded}); code != ExitFailure {
			t.Fatalf("run %d exit=%d stderr=%q", run+1, code, stderr.String())
		}
	}
	if outputCalls != 2 || stdout.Len() != 0 {
		t.Fatalf("output calls=%d stdout=%q", outputCalls, stdout.String())
	}
}

func newGetOutputPreparationCapability(t *testing.T, relays ...string) link.Link {
	t.Helper()
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x5a}, ed25519.SeedSize))
	capability, err := link.NewSenderAuthenticated(
		bytes.Repeat([]byte{0xa5}, link.ReadSecretBytes),
		privateKey.Public().(ed25519.PublicKey),
		relays,
	)
	if err != nil {
		t.Fatal(err)
	}
	return capability
}
