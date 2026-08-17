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

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/cmd/windshare/internal/runtrace"
	"github.com/windshare/windshare/cmd/windshare/internal/terminalcanvas"
	"github.com/windshare/windshare/core/link"
)

type bindingOnlyGetOutputAuthority struct {
	getOutputAuthority
	events *[]string
}

func (authority *bindingOnlyGetOutputAuthority) BindDestination(context.Context) (getOutputMode, error) {
	*authority.events = append(*authority.events, "bind")
	return getOutputResumable, nil
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
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: stderr, Stdin: strings.NewReader("")}
	request, parse := app.parseGetRequest([]string{encoded})
	if parse != requestParseReady || request.outDir != "." {
		t.Fatalf("request=%+v parse=%d stderr=%q", request, parse, stderr.String())
	}

	var (
		config getOutputAuthorityConfig
		events []string
	)
	app.getOutputFactory = getOutputAuthorityFactoryFunc(func(candidate getOutputAuthorityConfig) (getOutputAuthority, error) {
		config = candidate
		events = append(events, "construct")
		return &bindingOnlyGetOutputAuthority{events: &events}, nil
	})
	runtime, err := app.newCommandRuntime(clievent.CommandGet, observationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	prepared, code := app.prepareGetOutput(context.Background(), request, getObservation{runtime: runtime})
	if code != ExitOK {
		t.Fatalf("prepare exit=%d stderr=%q", code, stderr.String())
	}
	wantRoot, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	if config.rootPath != wantRoot || !config.createRoot || config.tracer == nil {
		t.Fatalf("output config=%+v want_root=%q", config, wantRoot)
	}
	if strings.Join(events, ",") != "construct,bind" {
		t.Fatalf("pre-session events=%v", events)
	}
	if err := prepared.authority.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != "construct,bind,close" {
		t.Fatalf("authority lifecycle=%v", events)
	}
	runtime.Close()
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("preparation wrote stdout=%q stderr=%q", stdout.String(), stderr.String())
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
		getOutputFactory: getOutputAuthorityFactoryFunc(func(getOutputAuthorityConfig) (getOutputAuthority, error) {
			outputCalls++
			return nil, errors.New("output construction must not run")
		}),
	}
	if code := app.runGet(t.Context(), []string{"--trace", filepath.Join(t.TempDir(), "get.ndjson"), encoded}); code != ExitFailure {
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
	traceDirectory := t.TempDir()
	var stdout, stderr bytes.Buffer
	outputCalls := 0
	app := &App{
		Stdout: &stdout, Stderr: &stderr,
		getOutputFactory: getOutputAuthorityFactoryFunc(func(getOutputAuthorityConfig) (getOutputAuthority, error) {
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
		if code := app.runGet(t.Context(), []string{"--trace-dir", traceDirectory, encoded}); code != ExitFailure {
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
