package cli

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"io"
	"testing"

	"github.com/windshare/windshare/core/link"
)

func TestCapabilityPublicationBuildsExactInvariantPayload(t *testing.T) {
	capability := testShareCapability(t)
	full, err := capability.URL("https://windshare.example/app")
	if err != nil {
		t.Fatal(err)
	}
	bare, key, err := capability.SplitURL("https://windshare.example/app")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		split bool
		want  string
	}{
		{name: "complete", want: "Link: " + full + "\n"},
		{name: "split", split: true, want: "Bare link: " + bare + "\nKey: " + key + "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, presentation := range []string{"tty-default", "tty-verbose", "redirected-default", "redirected-verbose"} {
				payload, err := buildShareCapabilityPayload(capability, "https://windshare.example/app", test.split)
				if err != nil {
					t.Fatalf("%s: build payload: %v", presentation, err)
				}
				if got := string(payload); got != test.want {
					t.Fatalf("%s: payload = %q, want %q", presentation, got, test.want)
				}
			}
		})
	}
}

func TestCapabilityPublicationChecksOneCompleteWrite(t *testing.T) {
	payload := []byte("Bare link: https://example.invalid/share\nKey: secret\n")
	writer := &sharePublicationWriter{}
	if err := publishShareCapability(writer, payload); err != nil {
		t.Fatal(err)
	}
	if writer.calls != 1 || !bytes.Equal(writer.payload, payload) {
		t.Fatalf("writes = %d payload = %q", writer.calls, writer.payload)
	}

	for _, test := range []struct {
		name   string
		writer io.Writer
	}{
		{name: "nil", writer: nil},
		{name: "short", writer: shareShortWriter{}},
		{name: "failed", writer: shareFailedWriter{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := publishShareCapability(test.writer, payload); err == nil {
				t.Fatal("publication failure was accepted")
			}
		})
	}
}

func TestSharePublicationOrdersWarmupAndReadinessAfterStdout(t *testing.T) {
	var order []string
	err := executeSharePublication(sharePublicationPlan{
		buildPayload: func() ([]byte, error) {
			order = append(order, "build")
			return []byte("Link: capability\n"), nil
		},
		publishPayload: func(payload []byte) error {
			order = append(order, "stdout:"+string(payload))
			return nil
		},
		stopRuntime:       func() { order = append(order, "stop") },
		startRootPrefetch: func() { order = append(order, "prefetch") },
		publishPrivateReady: func() error {
			order = append(order, "private-ready")
			return nil
		},
		publishPublicReady: func() { order = append(order, "public-ready") },
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"build", "stdout:Link: capability\n", "prefetch", "private-ready", "public-ready"}
	if !equalShareStrings(order, want) {
		t.Fatalf("order = %q, want %q", order, want)
	}
}

func TestSharePublicationFailureNeverPublishesPublicReadyAndStdoutFailureDoesNotStart(t *testing.T) {
	for _, test := range []struct {
		name       string
		buildErr   error
		publishErr error
		privateErr error
		wantStage  sharePublicationStage
		wantOrder  []string
	}{
		{name: "encoding", buildErr: errors.New("encoding canary"), wantStage: sharePublicationBuildFailed, wantOrder: []string{"build", "stop"}},
		{name: "stdout", publishErr: io.ErrShortWrite, wantStage: sharePublicationWriteFailed, wantOrder: []string{"build", "stdout", "stop"}},
		{name: "private readiness", privateErr: errors.New("private trace canary"), wantStage: sharePublicationPrivateReadyFailed, wantOrder: []string{"build", "stdout", "prefetch", "private-ready", "stop"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var order []string
			err := executeSharePublication(sharePublicationPlan{
				buildPayload: func() ([]byte, error) {
					order = append(order, "build")
					return []byte("Link: capability\n"), test.buildErr
				},
				publishPayload: func([]byte) error {
					order = append(order, "stdout")
					return test.publishErr
				},
				stopRuntime:       func() { order = append(order, "stop") },
				startRootPrefetch: func() { order = append(order, "prefetch") },
				publishPrivateReady: func() error {
					order = append(order, "private-ready")
					return test.privateErr
				},
				publishPublicReady: func() { order = append(order, "public-ready") },
			})
			var failure *sharePublicationFailure
			if !errors.As(err, &failure) || failure.stage != test.wantStage {
				t.Fatalf("failure = %#v, want stage %d", err, test.wantStage)
			}
			if !equalShareStrings(order, test.wantOrder) {
				t.Fatalf("order = %q, want %q", order, test.wantOrder)
			}
		})
	}
}

func testShareCapability(t *testing.T) link.Link {
	t.Helper()
	seed := bytes.Repeat([]byte{0x5a}, ed25519.SeedSize)
	privateKey := ed25519.NewKeyFromSeed(seed)
	capability, err := link.NewSenderAuthenticated(
		bytes.Repeat([]byte{0xa5}, link.ReadSecretBytes),
		privateKey.Public().(ed25519.PublicKey),
		[]string{"wss://relay.example/ws/v2?forbidden=discarded"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return capability
}

type sharePublicationWriter struct {
	calls   int
	payload []byte
}

func (writer *sharePublicationWriter) Write(payload []byte) (int, error) {
	writer.calls++
	writer.payload = append([]byte(nil), payload...)
	return len(payload), nil
}

type shareShortWriter struct{}

func (shareShortWriter) Write(payload []byte) (int, error) { return len(payload) - 1, nil }

type shareFailedWriter struct{}

func (shareFailedWriter) Write([]byte) (int, error) { return 0, errors.New("stdout provider canary") }

func equalShareStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
