package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/testnetwork"
)

const v2ProcessTimeout = 30 * time.Second

const (
	v2ResumeFileCount       = 128
	v2ResumeFileBytes       = 512 << 10
	v2FailureLogStreamBytes = 4 << 10

	v2PionRelayCutPayloadBytes  int64 = 32 << 20
	v2PionRelayCutBlockBytes          = 64 << 10
	v2PionRelayCutPayloadSHA256       = "e09320c5b00b34bb704802136c599a95b3996332ba84d7c7f21112b6231b6bd0"
	v2PionModulePath                  = "github.com/pion/webrtc/v4"
)

var v2RelayAddress = regexp.MustCompile(`listening on ([^\s(]+)`)

type processBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *processBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(value)
}

func (buffer *processBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

func (buffer *processBuffer) diagnosticString() string {
	value := buffer.String()
	if len(value) <= v2FailureLogStreamBytes {
		return value
	}
	// Keep both causal startup output and the terminal tail without letting a
	// many-file cascade turn one failed assertion into an unbounded CI log.
	edgeBytes := v2FailureLogStreamBytes / 2
	omitted := len(value) - 2*edgeBytes
	return fmt.Sprintf("%s\n... %d bytes omitted ...\n%s", value[:edgeBytes], omitted, value[len(value)-edgeBytes:])
}

type v2Process struct {
	command  *exec.Cmd
	stdout   *processBuffer
	stderr   *processBuffer
	done     chan struct{}
	err      error
	stopOnce sync.Once
}

func startV2Process(t *testing.T, binary string, arguments ...string) *v2Process {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
	process := &v2Process{stdout: &processBuffer{}, stderr: &processBuffer{}, done: make(chan struct{})}
	process.command = exec.Command(binary, arguments...)
	process.command.Stdout, process.command.Stderr = process.stdout, process.stderr
	if err := testnetwork.StartGuardedProcess(process.command); err != nil {
		t.Fatalf("start %s: %v", filepath.Base(binary), err)
	}
	go func() {
		process.err = process.command.Wait()
		close(process.done)
	}()
	t.Cleanup(process.stop)
	return process
}

func (process *v2Process) stop() {
	process.stopOnce.Do(func() {
		select {
		case <-process.done:
			return
		default:
		}
		_ = process.command.Process.Kill()
		<-process.done
	})
}

func (process *v2Process) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-process.done:
		return process.err
	case <-time.After(v2ProcessTimeout):
		process.stop()
		t.Fatalf("process timeout; stdout=%q stderr=%q", process.stdout.String(), process.stderr.String())
		return context.DeadlineExceeded
	}
}

func waitV2Match(t *testing.T, process *v2Process, expression *regexp.Regexp, stream *processBuffer) string {
	t.Helper()
	deadline := time.Now().Add(v2ProcessTimeout)
	for time.Now().Before(deadline) {
		if match := expression.FindStringSubmatch(stream.String()); match != nil {
			return match[1]
		}
		select {
		case <-process.done:
			t.Fatalf("process exited before readiness: %v; stdout=%q stderr=%q", process.err, process.stdout.String(), process.stderr.String())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("readiness timeout; stdout=%q stderr=%q", process.stdout.String(), process.stderr.String())
	return ""
}

func TestV2ProcessProgressiveCatalogConcurrentReceiversAndSelection(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	root := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(filepath.Join(root, "nested", "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stable.txt"), []byte("root-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "a.txt"), []byte("selected-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "zero.bin"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	relayState := filepath.Join(t.TempDir(), "relay-state")
	relay := startV2Process(t, relayBin, "-listen", "127.0.0.1:0", "-state-dir", relayState)
	address := waitV2Match(t, relay, v2RelayAddress, relay.stderr)
	relayURL := "ws://" + address
	share := startV2Process(t, windshareBin, "share", root, "--relay", relayURL)
	linkExpression := regexp.MustCompile(`(?m)^Link: (\S+)$`)
	shareLink := waitV2Match(t, share, linkExpression, share.stdout)
	outputs := []string{t.TempDir(), t.TempDir()}
	receivers := make([]*v2Process, 0, len(outputs))
	for _, output := range outputs {
		receivers = append(receivers, startV2Process(t, windshareBin, "get", shareLink, "-o", output))
	}
	for index, receiver := range receivers {
		if err := receiver.wait(t); err != nil {
			t.Fatalf("receiver %d failed: %v; stdout=%q stderr=%q", index, err, receiver.stdout.String(), receiver.stderr.String())
		}
		assertV2File(t, filepath.Join(outputs[index], "tree", "stable.txt"), []byte("root-content"))
		assertV2File(t, filepath.Join(outputs[index], "tree", "nested", "a.txt"), []byte("selected-content"))
		assertV2File(t, filepath.Join(outputs[index], "tree", "zero.bin"), nil)
		if info, err := os.Stat(filepath.Join(outputs[index], "tree", "nested", "empty-dir")); err != nil || !info.IsDir() {
			t.Fatalf("receiver %d empty directory: info=%v err=%v", index, info, err)
		}
	}

	selectedOutput := t.TempDir()
	selected := startV2Process(
		t, windshareBin, "get", shareLink, "-o", selectedOutput, "--only", "tree/nested/a.txt",
	)
	if err := selected.wait(t); err != nil {
		t.Fatalf("selected receiver failed: %v; stdout=%q stderr=%q", err, selected.stdout.String(), selected.stderr.String())
	}
	assertV2File(t, filepath.Join(selectedOutput, "tree", "nested", "a.txt"), []byte("selected-content"))
	if _, err := os.Stat(filepath.Join(selectedOutput, "tree", "mutable.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unselected file exists or returned wrong error: %v", err)
	}

	if strings.Contains(share.stderr.String(), "manifest") || strings.Contains(share.stderr.String(), "/v1") {
		t.Fatalf("production share emitted retired vocabulary: %q", share.stderr.String())
	}
}

func TestV2ProcessTransfersExactPayloadOverPionAfterRelayCut(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	windshareSHA256 := v2FileSHA256(t, windshareBin)
	pionModule := v2BuildModule(t, windshareBin, v2PionModulePath)
	source := filepath.Join(t.TempDir(), "pion-relay-cut.bin")
	writeV2PatternFile(t, source, v2PionRelayCutPayloadBytes, v2PionRelayCutPayloadSHA256)

	proxy := startRelayCutProxy(t)
	relay := startV2Process(
		t,
		relayBin,
		"-listen", "127.0.0.1:0",
		"-relay-base-url", proxy.BaseURL(),
		"-state-dir", filepath.Join(t.TempDir(), "relay-state"),
	)
	relayAddress := waitV2Match(t, relay, v2RelayAddress, relay.stderr)
	if err := proxy.ForwardTo(relayAddress); err != nil {
		t.Fatal(err)
	}

	share := startV2Process(
		t,
		windshareBin,
		"share", source,
		"--relay", proxy.BaseURL(),
		"--block-size", fmt.Sprint(v2PionRelayCutBlockBytes),
	)
	shareLink := waitV2Match(t, share, regexp.MustCompile(`(?m)^Link: (\S+)$`), share.stdout)
	output := t.TempDir()
	receiver := startV2Process(t, windshareBin, "get", shareLink, "-o", output)
	directMarker := waitV2Match(
		t,
		receiver,
		regexp.MustCompile(`(?m)^(get: direct peer lane active)$`),
		receiver.stderr,
	)
	select {
	case <-receiver.done:
		t.Fatalf(
			"receiver completed before relay cut; stdout=%q stderr=%q",
			receiver.stdout.String(),
			receiver.stderr.String(),
		)
	default:
	}

	relayDownstream, proxyErr := proxy.CutAndWait()
	relay.stop()
	if proxyErr != nil {
		t.Fatalf("cut relay proxy: %v", proxyErr)
	}
	// The counter includes every downstream TCP byte, including HTTP and relay
	// framing. Suite-02 does not compress content, so a value below plaintext
	// size proves the relay could not have delivered the complete fixture.
	if relayDownstream == 0 || relayDownstream >= uint64(v2PionRelayCutPayloadBytes) {
		t.Fatalf(
			"relay downstream at cut = %d bytes, want a positive value below payload size %d",
			relayDownstream,
			v2PionRelayCutPayloadBytes,
		)
	}
	if err := receiver.wait(t); err != nil {
		t.Fatalf(
			"receiver failed after authenticated Pion activation and relay cut: %v; stdout=%q stderr=%q",
			err,
			receiver.stdout.String(),
			receiver.stderr.String(),
		)
	}

	outputPath := filepath.Join(output, filepath.Base(source))
	outputSHA256 := assertV2FileSHA256(
		t,
		outputPath,
		v2PionRelayCutPayloadBytes,
		v2PionRelayCutPayloadSHA256,
	)
	t.Logf(
		"D5_CLI_PION_V1 windshare_sha256=%s pion_module=%s direct_lane_active=%t relay_cut=%t relay_downstream_at_cut=%d payload_bytes=%d payload_sha256=%s",
		windshareSHA256,
		pionModule,
		directMarker != "",
		true,
		relayDownstream,
		v2PionRelayCutPayloadBytes,
		outputSHA256,
	)
}

func writeV2PatternFile(t *testing.T, filename string, size int64, wantSHA256 string) {
	t.Helper()
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	pattern := make([]byte, v2PionRelayCutBlockBytes)
	for index := range pattern {
		pattern[index] = byte(index)
	}
	digest := sha256.New()
	writer := io.MultiWriter(file, digest)
	remaining := size
	for remaining > 0 {
		next := min(remaining, int64(len(pattern)))
		written, writeErr := writer.Write(pattern[:int(next)])
		if writeErr != nil || written != int(next) {
			_ = file.Close()
			t.Fatalf("write Pion relay-cut fixture: bytes=%d err=%v", written, writeErr)
		}
		remaining -= next
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", digest.Sum(nil)); got != wantSHA256 {
		t.Fatalf("Pion relay-cut fixture SHA-256 = %s, want %s", got, wantSHA256)
	}
}

func assertV2FileSHA256(t *testing.T, filename string, wantBytes int64, wantSHA256 string) string {
	t.Helper()
	information, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if information.Size() != wantBytes {
		t.Fatalf("%s size = %d, want %d", filename, information.Size(), wantBytes)
	}
	got := v2FileSHA256(t, filename)
	if got != wantSHA256 {
		t.Fatalf("%s SHA-256 = %s, want %s", filename, got, wantSHA256)
	}
	return got
}

func v2FileSHA256(t *testing.T, filename string) string {
	t.Helper()
	file, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("hash %s: %v", filename, errors.Join(copyErr, closeErr))
	}
	return fmt.Sprintf("%x", digest.Sum(nil))
}

func v2BuildModule(t *testing.T, binary, modulePath string) string {
	t.Helper()
	information, err := buildinfo.ReadFile(binary)
	if err != nil {
		t.Fatalf("read build provenance from %s: %v", binary, err)
	}
	for _, dependency := range information.Deps {
		if dependency == nil || dependency.Path != modulePath {
			continue
		}
		actual := dependency
		if dependency.Replace != nil {
			actual = dependency.Replace
		}
		if actual.Version == "" {
			t.Fatalf("%s has no version provenance in %s", modulePath, binary)
		}
		return modulePath + "@" + actual.Version
	}
	t.Fatalf("%s is absent from %s build provenance", modulePath, binary)
	return ""
}

func assertV2File(t *testing.T, filename string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("%s = %q, want %q", filename, actual, expected)
	}
}

func TestV2ProcessResumesDurableOutputAfterReceiverCrash(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	root := filepath.Join(t.TempDir(), "resume-tree")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0x5a}, v2ResumeFileBytes)
	for index := range v2ResumeFileCount {
		name := filepath.Join(root, fmt.Sprintf("file-%03d.bin", index))
		if err := os.WriteFile(name, payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	relay := startV2Process(t, relayBin, "-listen", "127.0.0.1:0", "-state-dir", filepath.Join(t.TempDir(), "relay-state"))
	address := waitV2Match(t, relay, v2RelayAddress, relay.stderr)
	share := startV2Process(t, windshareBin, "share", root, "--relay", "ws://"+address)
	shareLink := waitV2Match(t, share, regexp.MustCompile(`(?m)^Link: (\S+)$`), share.stdout)
	output := t.TempDir()
	firstOutput := filepath.Join(output, "resume-tree", "file-000.bin")

	interrupted := startV2Process(t, windshareBin, "get", shareLink, "-o", output)
	waitV2PublishedFile(t, interrupted, firstOutput)
	interrupted.stop()

	resumed := startV2Process(t, windshareBin, "get", shareLink, "-o", output)
	if err := resumed.wait(t); err != nil {
		t.Fatalf(
			"resumed receiver failed: %v; receiver stdout=%q stderr=%q; sender stdout=%q stderr=%q; relay stdout=%q stderr=%q",
			err,
			resumed.stdout.diagnosticString(), resumed.stderr.diagnosticString(),
			share.stdout.diagnosticString(), share.stderr.diagnosticString(),
			relay.stdout.diagnosticString(), relay.stderr.diagnosticString(),
		)
	}
	if !strings.Contains(resumed.stderr.String(), "resumed a durable output session") {
		t.Fatalf("receiver did not report durable resume: %q", resumed.stderr.String())
	}
	assertV2File(t, firstOutput, payload)
	assertV2File(t, filepath.Join(output, "resume-tree", fmt.Sprintf("file-%03d.bin", v2ResumeFileCount-1)), payload)
}

func waitV2PublishedFile(t *testing.T, process *v2Process, filename string) {
	t.Helper()
	deadline := time.Now().Add(v2ProcessTimeout)
	for time.Now().Before(deadline) {
		if information, err := os.Stat(filename); err == nil && information.Size() == v2ResumeFileBytes {
			select {
			case <-process.done:
				t.Fatalf("receiver completed before the crash checkpoint; stdout=%q stderr=%q", process.stdout.String(), process.stderr.String())
			default:
				return
			}
		}
		select {
		case <-process.done:
			t.Fatalf("receiver exited before publishing the crash checkpoint: %v; stdout=%q stderr=%q", process.err, process.stdout.String(), process.stderr.String())
		default:
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("receiver did not publish the crash checkpoint; stdout=%q stderr=%q", process.stdout.String(), process.stderr.String())
}

func TestV2ProductionSourcesDoNotImportRetiredStack(t *testing.T) {
	root := repoRoot()
	production := []string{
		filepath.Join(root, "cmd", "windshare", "internal", "cli", "share.go"),
		filepath.Join(root, "cmd", "windshare", "internal", "cli", "get.go"),
		filepath.Join(root, "relay", "cmd", "wsrelay", "main.go"),
	}
	retired := []string{"core/manifest", "core/share", "transport/relay\"", "relay/protocol\"", "/v1", "max-manifest"}
	for _, filename := range production {
		encoded, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range retired {
			if strings.Contains(string(encoded), value) {
				t.Errorf("%s still contains retired production dependency %q", filename, value)
			}
		}
	}
}

func TestV2ProcessErrorIncludesDiagnostics(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	command := exec.Command(windshareBin, "get", "not-a-link")
	output, err := command.CombinedOutput()
	if err == nil || len(output) == 0 {
		t.Fatalf("invalid link result: err=%v output=%q", err, output)
	}
}
