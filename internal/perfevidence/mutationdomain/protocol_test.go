package mutationdomain

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/perfevidence"
)

const protocolTestTimeout = time.Second

type protocolTestFixture struct {
	session         *session
	requests        *bufio.Reader
	responses       io.WriteCloser
	requestReadPipe io.Closer
	responseRead    io.Closer
	kills           atomic.Int32
}

func newProtocolTestFixture() *protocolTestFixture {
	requestRead, requestWrite := io.Pipe()
	responseRead, responseWrite := io.Pipe()
	fixture := &protocolTestFixture{
		requests: requestReadPipe(requestRead), responses: responseWrite,
		requestReadPipe: requestRead, responseRead: responseRead,
	}
	fixture.session = &session{
		stdin: requestWrite, stdout: bufio.NewReader(responseRead), stdoutPipe: responseRead,
		resolveProcessID: func(namespaceID int) (int, error) { return 10_000 + namespaceID, nil },
		kill: func() error {
			fixture.kills.Add(1)
			return nil
		},
		wait: func() error { return nil }, shutdownAfter: 50 * time.Millisecond, sinkAfter: protocolTestTimeout,
	}
	return fixture
}

func requestReadPipe(reader io.Reader) *bufio.Reader {
	return bufio.NewReaderSize(reader, maximumProtocolLine)
}

func (fixture *protocolTestFixture) close() {
	_ = fixture.session.stdin.Close()
	_ = fixture.responses.Close()
	_ = fixture.requestReadPipe.Close()
	_ = fixture.responseRead.Close()
}

func (fixture *protocolTestFixture) serveCommand(
	outputName string,
	output []byte,
	headerError string,
	headerFatal bool,
	finishedNamespaceID int,
) error {
	var command request
	if err := readJSONLine(fixture.requests, &command); err != nil {
		return err
	}
	if command.Command.Executable == "" {
		return errors.New("test server received an empty command")
	}
	const namespaceID = 2
	if err := writeJSONLine(fixture.responses, response{
		Event: targetStartedEvent, NamespaceProcessID: namespaceID, ExitCode: -1,
	}); err != nil {
		return err
	}
	var acknowledgement request
	if err := readJSONLine(fixture.requests, &acknowledgement); err != nil {
		return err
	}
	if !acknowledgement.ProcessIDAcknowledged {
		return errors.New("test server did not receive the PID acknowledgement")
	}
	if finishedNamespaceID == 0 {
		finishedNamespaceID = namespaceID
	}
	frames := []frame{
		{Name: "stdout", Bytes: 0, SHA256: hashBytes(nil)},
		{Name: "stderr", Bytes: 0, SHA256: hashBytes(nil)},
	}
	if outputName != "" {
		frames = append(frames, frame{Name: outputName, Bytes: int64(len(output)), SHA256: hashBytes(output)})
	}
	if err := writeJSONLine(fixture.responses, response{
		Event: targetFinishedEvent, NamespaceProcessID: finishedNamespaceID,
		ExitCode: 0, StartedAt: time.Unix(1, 0), FinishedAt: time.Unix(2, 0),
		Error: headerError, Fatal: headerFatal, Frames: frames,
	}); err != nil {
		return err
	}
	if _, err := io.Copy(fixture.responses, bytes.NewReader(output)); err != nil {
		return err
	}
	return writeJSONLine(fixture.responses, response{Event: targetSettledEvent, ExitCode: 0})
}

func testCommand(outputs ...perfevidence.MutationOutput) perfevidence.MutationDomainCommand {
	return perfevidence.MutationDomainCommand{
		Executable: "admitted-test-target", Directory: "admitted-test-root", Outputs: outputs,
	}
}

func TestRunLinearizesSuccessfulCompletionBeforeLaterCancellation(t *testing.T) {
	fixture := newProtocolTestFixture()
	defer fixture.close()
	serverResult := make(chan error, 1)
	go func() { serverResult <- fixture.serveCommand("", nil, "", false, 0) }()
	ctx, cancel := context.WithCancel(context.Background())
	result, err := fixture.session.Run(ctx, testCommand(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ProcessID != 10_002 || result.ExitCode != 0 {
		t.Fatalf("result = %+v", result)
	}
	cancel()
	select {
	case err := <-serverResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(protocolTestTimeout):
		t.Fatal("protocol server did not settle")
	}
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	if kills := fixture.kills.Load(); kills != 0 {
		t.Fatalf("late cancellation killed a successful session %d times", kills)
	}
}

func TestRunPoisonsSessionWhenContainmentSettlementFails(t *testing.T) {
	fixture := newProtocolTestFixture()
	defer fixture.close()
	serverResult := make(chan error, 1)
	go func() { serverResult <- fixture.serveCommand("", nil, "namespace settlement failed", true, 0) }()
	_, err := fixture.session.Run(context.Background(), testCommand(), nil)
	if err == nil || !strings.Contains(err.Error(), "namespace settlement failed") {
		t.Fatalf("Run() error = %v", err)
	}
	if fixture.kills.Load() != 1 {
		t.Fatalf("fatal settlement kill count = %d", fixture.kills.Load())
	}
	if _, err := fixture.session.Run(context.Background(), testCommand(), nil); err == nil ||
		!strings.Contains(err.Error(), "closed") {
		t.Fatalf("reused poisoned session error = %v", err)
	}
	if serverErr := <-serverResult; serverErr != nil {
		t.Fatal(serverErr)
	}
}

func TestRunRejectsFinishedEventForDifferentNamespaceProcess(t *testing.T) {
	fixture := newProtocolTestFixture()
	defer fixture.close()
	serverResult := make(chan error, 1)
	go func() { serverResult <- fixture.serveCommand("", nil, "", false, 3) }()
	_, err := fixture.session.Run(context.Background(), testCommand(), nil)
	if err == nil || !strings.Contains(err.Error(), "completion event") {
		t.Fatalf("Run() error = %v", err)
	}
	if fixture.kills.Load() != 1 {
		t.Fatalf("identity mismatch kill count = %d", fixture.kills.Load())
	}
	_ = <-serverResult
}

type settlingSink struct {
	writeEntered   chan struct{}
	writeCancelled chan struct{}
	allowWriteEnd  chan struct{}
	commitEntered  chan struct{}
	allowCommitEnd chan struct{}
	content        []byte
	committed      bool
	writeOnce      sync.Once
	commitOnce     sync.Once
}

func (sink *settlingSink) WriteContext(ctx context.Context, content []byte) (int, error) {
	sink.writeOnce.Do(func() { close(sink.writeEntered) })
	<-ctx.Done()
	close(sink.writeCancelled)
	<-sink.allowWriteEnd
	return 0, context.Cause(ctx)
}

func (sink *settlingSink) Seal(ctx context.Context, _ int64, _ string) error {
	sink.commitOnce.Do(func() { close(sink.commitEntered) })
	<-ctx.Done()
	<-sink.allowCommitEnd
	return context.Cause(ctx)
}

func (*settlingSink) Abort(context.Context) error { return nil }

func TestRunWaitsForCancelledSinkWriteToSettle(t *testing.T) {
	fixture := newProtocolTestFixture()
	defer fixture.close()
	outputName := "protected-output"
	sink := &settlingSink{
		writeEntered: make(chan struct{}), writeCancelled: make(chan struct{}), allowWriteEnd: make(chan struct{}),
		commitEntered: make(chan struct{}), allowCommitEnd: make(chan struct{}),
	}
	serverResult := make(chan error, 1)
	go func() { serverResult <- fixture.serveCommand(outputName, []byte("payload"), "", false, 0) }()
	ctx, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() {
		_, err := fixture.session.Run(ctx, testCommand(perfevidence.MutationOutput{
			HostPath: outputName, MaxBytes: 1 << 20,
		}), map[string]perfevidence.MutationOutputSink{outputName: sink})
		runResult <- err
	}()
	<-sink.writeEntered
	cancel()
	<-sink.writeCancelled
	select {
	case err := <-runResult:
		t.Fatalf("Run returned before the cancelled sink settled: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(sink.allowWriteEnd)
	select {
	case err := <-runResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(protocolTestTimeout):
		t.Fatal("Run did not return after the sink settled")
	}
	_ = <-serverResult
}

type stalledWriteCloser struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (writer *stalledWriteCloser) Write([]byte) (int, error) {
	writer.once.Do(func() { close(writer.entered) })
	<-writer.release
	return 0, io.ErrClosedPipe
}

func (writer *stalledWriteCloser) Close() error {
	select {
	case <-writer.release:
	default:
		close(writer.release)
	}
	return nil
}

type stalledReadCloser struct {
	release chan struct{}
}

func (reader *stalledReadCloser) Read([]byte) (int, error) {
	<-reader.release
	return 0, io.ErrClosedPipe
}

func (reader *stalledReadCloser) Close() error {
	select {
	case <-reader.release:
	default:
		close(reader.release)
	}
	return nil
}

func TestConcurrentCloseCallersShareBoundedTerminalSettlement(t *testing.T) {
	input := &stalledWriteCloser{entered: make(chan struct{}), release: make(chan struct{})}
	output := &stalledReadCloser{release: make(chan struct{})}
	var kills atomic.Int32
	session := &session{
		stdin: input, stdout: bufio.NewReader(output), stdoutPipe: output,
		kill: func() error { kills.Add(1); return nil }, wait: func() error { return nil },
		shutdownAfter: 20 * time.Millisecond,
	}
	results := make(chan error, 2)
	go func() { results <- session.Close() }()
	<-input.entered
	go func() { results <- session.Close() }()
	first := <-results
	second := <-results
	if first == nil || second == nil || first.Error() != second.Error() {
		t.Fatalf("concurrent Close errors = %v / %v", first, second)
	}
	if kills.Load() != 1 {
		t.Fatalf("Close kill count = %d", kills.Load())
	}
}

func TestCloseJoinsActiveRunSinkSettlement(t *testing.T) {
	fixture := newProtocolTestFixture()
	defer fixture.close()
	outputName := "protected-output"
	sink := &settlingSink{
		writeEntered: make(chan struct{}), writeCancelled: make(chan struct{}), allowWriteEnd: make(chan struct{}),
		commitEntered: make(chan struct{}), allowCommitEnd: make(chan struct{}),
	}
	serverResult := make(chan error, 1)
	go func() { serverResult <- fixture.serveCommand(outputName, []byte("payload"), "", false, 0) }()
	runResult := make(chan error, 1)
	go func() {
		_, err := fixture.session.Run(context.Background(), testCommand(perfevidence.MutationOutput{
			HostPath: outputName, MaxBytes: 1 << 20,
		}), map[string]perfevidence.MutationOutputSink{outputName: sink})
		runResult <- err
	}()
	<-sink.writeEntered
	closeResult := make(chan error, 1)
	go func() { closeResult <- fixture.session.Close() }()
	<-sink.writeCancelled
	select {
	case err := <-closeResult:
		t.Fatalf("Close returned before the active sink settled: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(sink.allowWriteEnd)
	select {
	case <-runResult:
	case <-time.After(protocolTestTimeout):
		t.Fatal("active Run did not settle after its sink")
	}
	select {
	case <-closeResult:
	case <-time.After(protocolTestTimeout):
		t.Fatal("Close did not join the settled active Run")
	}
	_ = <-serverResult
}

func TestRunWaitingForProtocolGateObservesClose(t *testing.T) {
	session := &session{shutdownAfter: 20 * time.Millisecond, wait: func() error { return nil }}
	gate := session.protocolGate()
	gate <- struct{}{}
	runResult := make(chan error, 1)
	go func() {
		_, err := session.Run(context.Background(), testCommand(), nil)
		runResult <- err
	}()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-runResult:
		if err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("waiting Run error = %v", err)
		}
	case <-time.After(protocolTestTimeout):
		t.Fatal("waiting Run did not observe Close")
	}
	<-gate
}

func TestRewriteCommandRequiresMappedExecutableAndDirectory(t *testing.T) {
	hostRoot := filepath.Join(string(filepath.Separator), "retained", "root")
	privateRoot := filepath.Join(string(filepath.Separator), privateInputDirectory, "root")
	state := &helperState{privateRoot: string(filepath.Separator), pathMap: map[string]string{hostRoot: privateRoot}, outputs: map[string]string{}}
	command := testCommand()
	command.Executable = filepath.Join(hostRoot, "bin", "target")
	command.Directory = filepath.Join(hostRoot, "working")
	rewritten, err := state.rewriteCommand(command)
	if err != nil || rewritten.Executable != filepath.Join(privateRoot, "bin", "target") {
		t.Fatalf("mapped command = %+v, %v", rewritten, err)
	}
	for name, mutate := range map[string]func(*perfevidence.MutationDomainCommand){
		"executable": func(command *perfevidence.MutationDomainCommand) {
			command.Executable = filepath.Join(string(filepath.Separator), "proc", "self", "exe")
		},
		"directory": func(command *perfevidence.MutationDomainCommand) {
			command.Directory = filepath.Join(string(filepath.Separator), privateTemporaryDirectory)
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := command
			mutate(&candidate)
			if _, err := state.rewriteCommand(candidate); err == nil {
				t.Fatal("unmapped command was admitted")
			}
		})
	}
}

func TestRewriteCommandPrefersLongestCanonicalPathOperand(t *testing.T) {
	hostRoot := filepath.Join(t.TempDir(), "retained", "root")
	privateRoot := filepath.Join(t.TempDir(), privateInputDirectory, "root")
	state := &helperState{
		privateRoot: t.TempDir(),
		pathMap:     map[string]string{hostRoot: privateRoot},
		outputs:     map[string]string{},
		promoted:    map[string]*platformPromotedInput{},
	}
	output := filepath.Join(hostRoot, "artifacts", "result.bin")
	command := testCommand(perfevidence.MutationOutput{HostPath: output, MaxBytes: 1 << 20})
	command.Executable = filepath.Join(hostRoot, "bin", "target")
	command.Directory = filepath.Join(hostRoot, "working")
	command.Arguments = []string{"-o=" + output, "--literal=" + hostRoot + "-suffix"}
	rewritten, err := state.rewriteCommand(command)
	if err != nil {
		t.Fatal(err)
	}
	isolatedOutput := state.outputs[output]
	if rewritten.Arguments[0] != "-o="+isolatedOutput {
		t.Fatalf("nested output operand = %q, want %q", rewritten.Arguments[0], "-o="+isolatedOutput)
	}
	if rewritten.Arguments[1] != command.Arguments[1] {
		t.Fatalf("non-path substring was rewritten: %q", rewritten.Arguments[1])
	}
}

func TestRestoreCapturedPathsReturnsHostSemanticJSON(t *testing.T) {
	hostRoot := filepath.Join(t.TempDir(), "snapshot")
	hostCache := filepath.Join(t.TempDir(), "authoritative-cache")
	hostTemporary := filepath.Join(t.TempDir(), "authoritative-temporary")
	privateRoot := filepath.Join(t.TempDir(), "private")
	privateInput := filepath.Join(privateRoot, privateInputDirectory, "snapshot")
	state := &helperState{
		privateRoot: privateRoot,
		pathMap:     map[string]string{hostRoot: privateInput},
		outputs:     map[string]string{},
		promoted:    map[string]*platformPromotedInput{},
	}
	want := map[string]string{
		"source":    filepath.Join(hostRoot, "pkg", "source.go"),
		"generated": filepath.Join(hostCache, "generated", "_testmain.go"),
		"temporary": filepath.Join(hostTemporary, "work", "importcfg"),
	}
	private := map[string]string{
		"source":    filepath.Join(privateInput, "pkg", "source.go"),
		"generated": filepath.Join(privateRoot, privateCacheDirectory, "generated", "_testmain.go"),
		"temporary": filepath.Join(privateRoot, privateTemporaryDirectory, "work", "importcfg"),
	}
	encoded, err := json.Marshal(private)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := state.restoreCapturedPaths(encoded, perfevidence.MutationDomainCommand{Environment: []string{
		"GOCACHE=" + hostCache, "GOTMPDIR=" + hostTemporary,
	}})
	if err != nil {
		t.Fatal(err)
	}
	var observed map[string]string
	if err := json.Unmarshal(restored, &observed); err != nil {
		t.Fatal(err)
	}
	for name, expected := range want {
		if observed[name] != expected {
			t.Errorf("restored %s = %q, want %q", name, observed[name], expected)
		}
	}
}

func TestCapturedPathRewriteIsCanonicalAndTokenBounded(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	nested := filepath.Join(root, "nested")
	mappings := []helperPathMapping{
		{source: root, destination: "HOST_ROOT"},
		{source: nested, destination: "HOST_NESTED"},
	}
	input := nested + string(filepath.Separator) + "file " + root + string(filepath.Separator) + "file buildid=prefix" + root + "suffix"
	want := "HOST_NESTED" + string(filepath.Separator) + "file HOST_ROOT" + string(filepath.Separator) + "file buildid=prefix" + root + "suffix"
	for iteration := 0; iteration < 10; iteration++ {
		observed, err := rewritePathMappings(input, mappings, 0)
		if err != nil {
			t.Fatal(err)
		}
		if observed != want {
			t.Fatalf("canonical rewrite = %q, want %q", observed, want)
		}
	}
}

func TestCapturedPathRewritePreservesNonASCIIText(t *testing.T) {
	source := filepath.Join(t.TempDir(), "缓存", "目录")
	destination := filepath.Join(t.TempDir(), "主机", "源码")
	input := "错误: " + filepath.Join(source, "文件.go")
	want := "错误: " + filepath.Join(destination, "文件.go")
	observed, err := rewriteCapturedPathBytes(
		[]byte(input), []helperPathMapping{{source: source, destination: destination}}, maximumCapturedBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(observed) != want {
		t.Fatalf("non-ASCII rewrite = %q, want %q", observed, want)
	}
}

func TestCapturedPathRewritePreservesInvalidUTF8Bytes(t *testing.T) {
	content := []byte{0xff, ' ', '/', 'p', 'r', 'i', 'v', 'a', 't', 'e', 0xfe}
	observed, err := rewriteCapturedPathBytes(
		content, []helperPathMapping{{source: "/private", destination: "/host"}}, len(content),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(observed, content) {
		t.Fatalf("invalid UTF-8 diagnostic changed: %x != %x", observed, content)
	}
}

func TestCapturedPathRewriteHonorsExactExpansionBound(t *testing.T) {
	mapping := []helperPathMapping{{source: "/p", destination: "/expanded"}}
	observed, err := rewriteCapturedPathBytes([]byte("/p"), mapping, len("/expanded"))
	if err != nil || string(observed) != "/expanded" {
		t.Fatalf("exact-bound rewrite = %q, %v", observed, err)
	}
	if _, err := rewriteCapturedPathBytes([]byte("/p"), mapping, len("/expanded")-1); err == nil {
		t.Fatal("path expansion beyond the capture bound was accepted")
	}
}

type shortWriter struct{}

func (shortWriter) Write(content []byte) (int, error) {
	if len(content) == 0 {
		return 0, nil
	}
	return len(content) - 1, nil
}

func TestWriteJSONLineRejectsShortWrites(t *testing.T) {
	if err := writeJSONLine(shortWriter{}, response{ExitCode: 0}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("writeJSONLine() error = %v", err)
	}
}

func TestProtocolTestFixtureFormatting(t *testing.T) {
	// This keeps fmt imported for readable failures in helper extensions.
	if got := fmt.Sprint(targetStartedEvent); got == "" {
		t.Fatal("empty event")
	}
}
