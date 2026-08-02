package mutationdomain

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/windshare/windshare/internal/perfevidence"
)

type helperState struct {
	privateRoot string
	pathMap     map[string]string
	outputs     map[string]string
	promoted    map[string]*platformPromotedInput
	outputRoot  *platformOutputAuthority
	generation  uint64
	cleanup     func() error
}

type frameSource struct {
	description frame
	reader      io.Reader
	close       func() error
	promote     func() (*platformPromotedInput, error)
}

type targetCapture struct {
	stdout      limitedBuffer
	stderr      limitedBuffer
	stdoutRead  *os.File
	stderrRead  *os.File
	stdoutWrite *os.File
	stderrWrite *os.File
	stdoutDone  <-chan error
	stderrDone  <-chan error
}

func prepareTargetCapture(process *exec.Cmd) (*targetCapture, error) {
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, stdoutRead.Close(), stdoutWrite.Close())
	}
	capture := &targetCapture{
		stdoutRead: stdoutRead, stderrRead: stderrRead,
		stdoutWrite: stdoutWrite, stderrWrite: stderrWrite,
	}
	capture.stdout.limit = maximumCapturedBytes
	capture.stderr.limit = maximumCapturedBytes
	copyPipe := func(reader *os.File, destination io.Writer) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, copyErr := io.Copy(destination, reader)
			done <- errors.Join(copyErr, reader.Close())
		}()
		return done
	}
	capture.stdoutDone = copyPipe(stdoutRead, &capture.stdout)
	capture.stderrDone = copyPipe(stderrRead, &capture.stderr)
	process.Stdout = stdoutWrite
	process.Stderr = stderrWrite
	return capture, nil
}

func (capture *targetCapture) releaseWriters() error {
	if capture == nil {
		return nil
	}
	var errs []error
	if capture.stdoutWrite != nil {
		errs = append(errs, capture.stdoutWrite.Close())
		capture.stdoutWrite = nil
	}
	if capture.stderrWrite != nil {
		errs = append(errs, capture.stderrWrite.Close())
		capture.stderrWrite = nil
	}
	return errors.Join(errs...)
}

func (capture *targetCapture) settle() error {
	if capture == nil {
		return nil
	}
	releaseErr := capture.releaseWriters()
	timer := time.NewTimer(captureSettlementTimeout)
	defer timer.Stop()
	var errs []error
	doneChannels := []<-chan error{capture.stdoutDone, capture.stderrDone}
	for index, done := range doneChannels {
		select {
		case err := <-done:
			errs = append(errs, err)
		case <-timer.C:
			closeErr := errors.Join(capture.stdoutRead.Close(), capture.stderrRead.Close())
			for _, remaining := range doneChannels[index:] {
				errs = append(errs, <-remaining)
			}
			return errors.Join(
				releaseErr, closeErr, errors.Join(errs...),
				errors.New("isolated target capture settlement exceeded its bound"),
			)
		}
	}
	return errors.Join(releaseErr, errors.Join(errs...))
}

func MaybeRunHelper(arguments []string, stdin io.Reader, stdout, stderr io.Writer) (bool, int) {
	if handled, code := maybeRunPlatformTarget(arguments, stderr); handled {
		return true, code
	}
	if handled, code := maybeRunPlatformBroker(arguments, stdin, stdout, stderr); handled {
		return true, code
	}
	argumentRole := len(arguments) == 1 && arguments[0] == helperArgument
	environmentRole := os.Getenv(helperRoleEnvironment) == "1"
	if !argumentRole && !environmentRole {
		return false, 0
	}
	if err := runHelper(stdin, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "private mutation helper failed: %v\n", err)
		return true, 1
	}
	return true, 0
}

func runHelper(stdin io.Reader, stdout io.Writer) (resultErr error) {
	reader := bufio.NewReaderSize(stdin, maximumProtocolLine)
	var configuration initialization
	if err := readJSONLine(reader, &configuration); err != nil {
		return fmt.Errorf("read initialization: %w", err)
	}
	state, err := initializeHelper(configuration)
	if err != nil {
		_ = writeJSONLine(stdout, response{Error: err.Error(), ExitCode: -1})
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, state.cleanup()) }()
	if err := writeJSONLine(stdout, response{ExitCode: 0}); err != nil {
		return err
	}
	for {
		var commandRequest request
		if err := readJSONLine(reader, &commandRequest); err != nil {
			return err
		}
		if commandRequest.Shutdown {
			cleanupErr := state.cleanup()
			state.cleanup = func() error { return nil }
			responseHeader := response{ExitCode: 0}
			if cleanupErr != nil {
				responseHeader.Error = cleanupErr.Error()
			}
			return writeJSONLine(stdout, responseHeader)
		}
		if commandRequest.ProcessIDAcknowledged {
			return errors.New("unexpected target process acknowledgement")
		}
		if err := state.runCommand(commandRequest.Command, reader, stdout); err != nil {
			return err
		}
	}
}

func initializeHelper(configuration initialization) (*helperState, error) {
	privateRoot, sources, cleanup, err := preparePlatformHelper(configuration)
	if err != nil {
		return nil, err
	}
	fail := func(operationErr error) (*helperState, error) {
		return nil, errors.Join(operationErr, cleanup())
	}
	state := &helperState{
		privateRoot: privateRoot,
		pathMap:     make(map[string]string, len(configuration.Roots)),
		outputs:     make(map[string]string),
		promoted:    make(map[string]*platformPromotedInput),
		cleanup:     cleanup,
	}
	traversalBudget := productionMutationTraversalBudget()
	for _, root := range configuration.Roots {
		if err := traversalBudget.admitCandidate(root.Name); err != nil {
			return fail(fmt.Errorf("admit retained input root %s: %w", root.Name, err))
		}
		source := sources[root.Name]
		if source == "" {
			return fail(fmt.Errorf("platform did not retain input root %s", root.Name))
		}
		destination := filepath.Join(privateRoot, privateInputDirectory, root.Name)
		var observed string
		if filepath.Clean(source) != filepath.Clean(destination) {
			observed, err = copyTreeWithBudget(source, destination, traversalBudget)
			if err != nil {
				return fail(fmt.Errorf("copy leased input root %s: %w", root.Name, err))
			}
		} else {
			observed, err = treeSHA256WithBudget(destination, traversalBudget)
		}
		if err != nil || observed != root.SHA256 {
			return fail(errors.Join(
				fmt.Errorf("copied input root %s has identity %s, want %s", root.Name, observed, root.SHA256),
				err,
			))
		}
		state.pathMap[filepath.Clean(root.HostPath)] = destination
	}
	for _, directory := range []string{
		filepath.Join(privateRoot, privateOutputDirectory),
		filepath.Join(privateRoot, privateCacheDirectory),
		filepath.Join(privateRoot, privateTemporaryDirectory),
		filepath.Join(privateRoot, privatePromotedDirectory),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fail(err)
		}
	}
	outputRoot, err := openPlatformOutputAuthority(filepath.Join(privateRoot, privateOutputDirectory))
	if err != nil {
		return fail(fmt.Errorf("retain private output directory: %w", err))
	}
	state.outputRoot = outputRoot
	platformCleanup := state.cleanup
	state.cleanup = func() error {
		var promotedErrs []error
		for _, input := range state.promoted {
			promotedErrs = append(promotedErrs, input.close())
		}
		state.promoted = nil
		return errors.Join(errors.Join(promotedErrs...), outputRoot.close(), platformCleanup())
	}
	return state, nil
}

func (state *helperState) runCommand(
	command perfevidence.MutationDomainCommand,
	reader *bufio.Reader,
	writer io.Writer,
) (resultErr error) {
	rewritten, err := state.rewriteCommand(command)
	if err != nil {
		return writeRejectedCommand(writer, err)
	}
	executable, arguments := platformTargetInvocation(rewritten.Executable, rewritten.Arguments)
	process := exec.Command(executable, arguments...)
	process.Dir = rewritten.Directory
	process.Env = rewritten.Environment
	process.SysProcAttr = helperTargetProcessAttributes()
	capture, err := prepareTargetCapture(process)
	if err != nil {
		return writeJSONLine(writer, response{Event: targetFinishedEvent, Error: err.Error(), Fatal: true, ExitCode: -1})
	}
	afterStart, releaseTarget, closeTargetGate, err := preparePlatformTarget(process)
	if err != nil {
		return errors.Join(
			writeJSONLine(writer, response{Event: targetFinishedEvent, Error: err.Error(), Fatal: true, ExitCode: -1}),
			capture.settle(),
		)
	}
	gateClosed := false
	startErr := process.Start()
	processStarted := startErr == nil
	startErr = errors.Join(startErr, capture.releaseWriters())
	processID := 0
	var startedAt time.Time
	targetReleased := false
	if processStarted {
		processID = process.Process.Pid
		startErr = afterStart()
	}
	fatalErr := startErr
	if startErr == nil {
		if err := writeJSONLine(writer, response{
			Event: targetStartedEvent, NamespaceProcessID: processID, ExitCode: -1,
		}); err != nil {
			fatalErr = err
		} else {
			var acknowledgement request
			if err := readJSONLine(reader, &acknowledgement); err != nil || !acknowledgement.ProcessIDAcknowledged ||
				acknowledgement.Shutdown || acknowledgement.Command.Executable != "" {
				fatalErr = errors.Join(errors.New("invalid target process acknowledgement"), err)
			} else {
				startedAt = time.Now().UTC()
				fatalErr = releaseTarget()
				gateClosed = true
				targetReleased = fatalErr == nil
			}
		}
	}
	if !gateClosed {
		fatalErr = errors.Join(fatalErr, closeTargetGate())
		gateClosed = true
	}
	if fatalErr != nil && process.Process != nil {
		killErr := process.Process.Kill()
		if !errors.Is(killErr, os.ErrProcessDone) {
			fatalErr = errors.Join(fatalErr, killErr)
		}
	}
	var commandErr error
	if processStarted {
		waitErr := process.Wait()
		var exitErr *exec.ExitError
		if fatalErr == nil && errors.As(waitErr, &exitErr) {
			commandErr = waitErr
		} else {
			fatalErr = errors.Join(fatalErr, waitErr)
		}
	}
	finishedAt := time.Now().UTC()
	var settlementErr error
	if processStarted {
		settlementErr = settlePlatformTarget()
		fatalErr = errors.Join(fatalErr, settlementErr)
	}
	fatalErr = errors.Join(fatalErr, capture.settle())
	exitCode := -1
	if process.ProcessState != nil {
		exitCode = process.ProcessState.ExitCode()
	}
	if capture.stdout.exceeded() || capture.stderr.exceeded() {
		fatalErr = errors.Join(fatalErr, errors.New("isolated command output exceeded its bounded capture"))
	}
	stdoutContent := capture.stdout.snapshot()
	stderrContent := capture.stderr.snapshot()
	if command.RestorePaths {
		var restoreErr error
		stdoutContent, restoreErr = state.restoreCapturedPaths(stdoutContent, command)
		fatalErr = errors.Join(fatalErr, restoreErr)
		stderrContent, restoreErr = state.restoreCapturedPaths(stderrContent, command)
		fatalErr = errors.Join(fatalErr, restoreErr)
	}
	sources := []frameSource{
		bytesFrame("stdout", stdoutContent),
		bytesFrame("stderr", stderrContent),
	}
	producerSucceeded := targetReleased && fatalErr == nil && settlementErr == nil && commandErr == nil && exitCode == 0
	if producerSucceeded {
		for _, output := range command.Outputs {
			source, outputErr := state.outputFrame(output)
			if outputErr != nil {
				fatalErr = errors.Join(fatalErr, outputErr)
				break
			}
			sources = append(sources, source)
		}
	}
	if !producerSucceeded || fatalErr != nil {
		for _, source := range sources[2:] {
			if source.close != nil {
				fatalErr = errors.Join(fatalErr, source.close())
			}
		}
		sources = sources[:2]
		fatalErr = errors.Join(fatalErr, state.discardCommandOutputs(command.Outputs))
	}
	header := response{
		Event: targetFinishedEvent, NamespaceProcessID: processID,
		ExitCode: exitCode, StartedAt: startedAt, FinishedAt: finishedAt,
		Frames: make([]frame, len(sources)),
	}
	headerError := errors.Join(commandErr, fatalErr)
	if headerError != nil {
		header.Error = headerError.Error()
	}
	header.Fatal = fatalErr != nil
	for index := range sources {
		header.Frames[index] = sources[index].description
	}
	if err := writeJSONLine(writer, header); err != nil {
		return err
	}
	for _, source := range sources {
		if _, err := io.CopyN(writer, source.reader, source.description.Bytes); err != nil {
			return err
		}
	}
	var promotionErrs []error
	for _, source := range sources {
		if source.promote == nil {
			continue
		}
		input, err := source.promote()
		if err != nil {
			promotionErrs = append(promotionErrs, err)
			continue
		}
		if previous := state.promoted[source.description.Name]; previous != nil {
			promotionErrs = append(promotionErrs, errors.Join(
				fmt.Errorf("immutable promoted output %s already exists", source.description.Name), input.close(),
			))
			continue
		}
		state.promoted[source.description.Name] = input
		delete(state.outputs, source.description.Name)
	}
	var closeErrs []error
	for _, source := range sources {
		if source.close != nil {
			closeErrs = append(closeErrs, source.close())
		}
	}
	closeErr := errors.Join(errors.Join(promotionErrs...), errors.Join(closeErrs...))
	trailer := response{Event: targetSettledEvent, ExitCode: exitCode, Fatal: closeErr != nil}
	if closeErr != nil {
		trailer.Error = closeErr.Error()
	}
	writeErr := writeJSONLine(writer, trailer)
	return errors.Join(fatalErr, closeErr, writeErr)
}

func (state *helperState) rewriteCommand(command perfevidence.MutationDomainCommand) (
	perfevidence.MutationDomainCommand,
	error,
) {
	result := command
	var admitted bool
	result.Executable, admitted = state.rewriteInputPath(command.Executable)
	if !admitted {
		return perfevidence.MutationDomainCommand{}, fmt.Errorf(
			"isolated executable %s is outside the admitted immutable inputs", command.Executable,
		)
	}
	result.Directory, admitted = state.rewriteInputPath(command.Directory)
	if !admitted {
		return perfevidence.MutationDomainCommand{}, fmt.Errorf(
			"isolated working directory %s is outside the admitted immutable inputs", command.Directory,
		)
	}
	commandOutputs := make(map[string]string, len(command.Outputs))
	for _, output := range command.Outputs {
		hostPath := filepath.Clean(output.HostPath)
		if !filepath.IsAbs(hostPath) || hostPath != output.HostPath || output.MaxBytes < 0 {
			return perfevidence.MutationDomainCommand{}, fmt.Errorf("isolated output path %q is not canonical", output.HostPath)
		}
		if state.generation == ^uint64(0) {
			return perfevidence.MutationDomainCommand{}, errors.New("isolated output generation space was exhausted")
		}
		isolated := filepath.Join(
			state.privateRoot, privateOutputDirectory,
			hashBytes([]byte(fmt.Sprintf("%s\x00%d", hostPath, state.generation))),
		)
		state.generation++
		if _, exists := commandOutputs[hostPath]; exists {
			return perfevidence.MutationDomainCommand{}, fmt.Errorf("isolated output %s is duplicated", hostPath)
		}
		if _, exists := state.outputs[hostPath]; exists {
			return perfevidence.MutationDomainCommand{}, fmt.Errorf("isolated output %s was already produced", hostPath)
		}
		if _, exists := state.promoted[hostPath]; exists {
			return perfevidence.MutationDomainCommand{}, fmt.Errorf("immutable output %s was already promoted", hostPath)
		}
		commandOutputs[hostPath] = isolated
	}
	result.Arguments = append([]string(nil), command.Arguments...)
	for index := range result.Arguments {
		result.Arguments[index] = state.rewriteText(result.Arguments[index], commandOutputs)
	}
	result.Environment = make([]string, 0, len(command.Environment)+1)
	mutable := map[string]string{
		"GOCACHE":  filepath.Join(state.privateRoot, privateCacheDirectory),
		"GOTMPDIR": filepath.Join(state.privateRoot, privateTemporaryDirectory),
		"TEMP":     filepath.Join(state.privateRoot, privateTemporaryDirectory),
		"TMP":      filepath.Join(state.privateRoot, privateTemporaryDirectory),
		"TMPDIR":   filepath.Join(state.privateRoot, privateTemporaryDirectory),
	}
	seen := make(map[string]bool)
	for _, entry := range command.Environment {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			return perfevidence.MutationDomainCommand{}, fmt.Errorf("invalid isolated environment entry %q", entry)
		}
		if replacement, ok := mutable[strings.ToUpper(name)]; ok {
			value = replacement
			seen[strings.ToUpper(name)] = true
		} else {
			value = state.rewriteText(value, commandOutputs)
		}
		result.Environment = append(result.Environment, name+"="+value)
	}
	for name, value := range mutable {
		if !seen[name] {
			result.Environment = append(result.Environment, name+"="+value)
		}
	}
	sort.Strings(result.Environment)
	for hostPath, isolated := range commandOutputs {
		state.outputs[hostPath] = isolated
	}
	return result, nil
}

func (state *helperState) rewriteInputPath(path string) (string, bool) {
	clean := filepath.Clean(path)
	if promoted := state.promoted[clean]; promoted != nil {
		return promoted.path(), true
	}
	bestSource := ""
	bestDestination := ""
	bestRelative := ""
	for source, destination := range state.pathMap {
		relative, err := filepath.Rel(source, clean)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		if len(source) > len(bestSource) {
			bestSource = source
			bestDestination = destination
			bestRelative = relative
		}
	}
	if bestSource == "" {
		return "", false
	}
	return filepath.Join(bestDestination, bestRelative), true
}

type helperPathMapping struct {
	source      string
	destination string
}

func (state *helperState) rewriteText(value string, additions map[string]string) string {
	bySource := make(map[string]helperPathMapping, len(state.pathMap)+len(state.promoted)+len(state.outputs)+len(additions))
	add := func(source, destination string) {
		key := source
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		bySource[key] = helperPathMapping{source: source, destination: destination}
	}
	for source, destination := range state.pathMap {
		add(source, destination)
	}
	for source, input := range state.promoted {
		add(source, input.path())
	}
	for source, destination := range state.outputs {
		add(source, destination)
	}
	for source, destination := range additions {
		add(source, destination)
	}
	mappings := make([]helperPathMapping, 0, len(bySource))
	for _, mapping := range bySource {
		mappings = append(mappings, mapping)
	}
	result, _ := rewritePathMappings(value, mappings, 0)
	return result
}

func (state *helperState) restoreCapturedPaths(
	content []byte,
	command perfevidence.MutationDomainCommand,
) ([]byte, error) {
	bySource := make(map[string]helperPathMapping, len(state.pathMap)+len(state.promoted)+len(state.outputs)+2)
	add := func(source, destination string) {
		if source == "" || destination == "" {
			return
		}
		key := source
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		bySource[key] = helperPathMapping{source: source, destination: destination}
		if runtime.GOOS == "windows" {
			escapedSource := strings.ReplaceAll(source, `\`, `\\`)
			escapedDestination := strings.ReplaceAll(destination, `\`, `\\`)
			bySource[strings.ToLower(escapedSource)] = helperPathMapping{
				source: escapedSource, destination: escapedDestination,
			}
			forwardSource := filepath.ToSlash(source)
			bySource[strings.ToLower(forwardSource)] = helperPathMapping{
				source: forwardSource, destination: filepath.ToSlash(destination),
			}
		}
	}
	for source, destination := range state.pathMap {
		add(destination, source)
	}
	for source, input := range state.promoted {
		add(input.path(), source)
	}
	for source, destination := range state.outputs {
		add(destination, source)
	}
	environment := make(map[string]string, len(command.Environment))
	for _, entry := range command.Environment {
		name, value, found := strings.Cut(entry, "=")
		if found {
			environment[strings.ToUpper(name)] = value
		}
	}
	add(filepath.Join(state.privateRoot, privateCacheDirectory), environment["GOCACHE"])
	temporary := environment["GOTMPDIR"]
	if temporary == "" {
		for _, name := range []string{"TEMP", "TMP", "TMPDIR"} {
			if environment[name] != "" {
				temporary = environment[name]
				break
			}
		}
	}
	add(filepath.Join(state.privateRoot, privateTemporaryDirectory), temporary)
	mappings := make([]helperPathMapping, 0, len(bySource))
	for _, mapping := range bySource {
		mappings = append(mappings, mapping)
	}
	restored, err := rewriteCapturedPathBytes(content, mappings, maximumCapturedBytes)
	if err != nil {
		return content, fmt.Errorf("restore isolated output path semantics: %w", err)
	}
	return restored, nil
}

func rewriteCapturedPathBytes(content []byte, mappings []helperPathMapping, maximumBytes int) ([]byte, error) {
	if maximumBytes > 0 && len(content) > maximumBytes {
		return nil, errors.New("captured isolated output exceeded its bound")
	}
	// Diagnostics are byte streams. Invalid UTF-8 cannot safely participate in
	// semantic path rewriting, so preserve the already-bounded evidence exactly.
	if !utf8.Valid(content) {
		return bytes.Clone(content), nil
	}
	rewritten, err := rewritePathMappings(string(content), mappings, maximumBytes)
	if err != nil {
		return nil, err
	}
	return []byte(rewritten), nil
}

func rewritePathMappings(value string, mappings []helperPathMapping, maximumBytes int) (string, error) {
	if !utf8.ValidString(value) {
		return "", errors.New("path rewrite input is not valid UTF-8")
	}
	ordered := append([]helperPathMapping(nil), mappings...)
	sort.Slice(ordered, func(left, right int) bool {
		if len(ordered[left].source) != len(ordered[right].source) {
			return len(ordered[left].source) > len(ordered[right].source)
		}
		if ordered[left].source != ordered[right].source {
			return ordered[left].source < ordered[right].source
		}
		return ordered[left].destination < ordered[right].destination
	})
	canonical := ordered[:0]
	seen := make(map[string]struct{}, len(ordered))
	for _, mapping := range ordered {
		if mapping.source == "" || !utf8.ValidString(mapping.source) || !utf8.ValidString(mapping.destination) {
			continue
		}
		key := mapping.source
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		canonical = append(canonical, mapping)
	}
	var result strings.Builder
	result.Grow(len(value))
	for offset := 0; offset < len(value); {
		matched := false
		for _, mapping := range canonical {
			if !pathMappingMatches(value, offset, mapping.source) {
				continue
			}
			if maximumBytes > 0 && result.Len()+len(mapping.destination) > maximumBytes {
				return "", errors.New("restored isolated output exceeded its bound")
			}
			result.WriteString(mapping.destination)
			offset += len(mapping.source)
			matched = true
			break
		}
		if !matched {
			_, width := utf8.DecodeRuneInString(value[offset:])
			if maximumBytes > 0 && result.Len()+width > maximumBytes {
				return "", errors.New("restored isolated output exceeded its bound")
			}
			result.WriteString(value[offset : offset+width])
			offset += width
		}
	}
	return result.String(), nil
}

func pathMappingMatches(value string, offset int, source string) bool {
	if source == "" || offset < 0 || len(value)-offset < len(source) {
		return false
	}
	if !utf8.RuneStart(value[offset]) {
		return false
	}
	candidate := value[offset : offset+len(source)]
	if runtime.GOOS == "windows" {
		if !strings.EqualFold(candidate, source) {
			return false
		}
	} else if candidate != source {
		return false
	}
	if offset > 0 && !pathOperandBoundary(value[offset-1]) {
		return false
	}
	end := offset + len(source)
	if end < len(value) && !utf8.RuneStart(value[end]) {
		return false
	}
	if end == len(value) || os.IsPathSeparator(source[len(source)-1]) {
		return true
	}
	return os.IsPathSeparator(value[end]) || pathOperandBoundary(value[end])
}

func pathOperandBoundary(character byte) bool {
	if character <= ' ' {
		return true
	}
	switch character {
	case '=', ',', ';', ':', '"', '\'', '[', ']', '(', ')', '{', '}':
		return true
	default:
		return false
	}
}

func (state *helperState) discardCommandOutputs(outputs []perfevidence.MutationOutput) error {
	var errs []error
	for _, output := range outputs {
		hostPath := filepath.Clean(output.HostPath)
		isolated := state.outputs[hostPath]
		delete(state.outputs, hostPath)
		if isolated == "" {
			continue
		}
		if err := os.Remove(isolated); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("discard failed isolated output %s: %w", hostPath, err))
		}
	}
	return errors.Join(errs...)
}

func (state *helperState) outputFrame(output perfevidence.MutationOutput) (frameSource, error) {
	path := state.outputs[output.HostPath]
	leaf := filepath.Base(path)
	if leaf == "." || filepath.Dir(path) != filepath.Join(state.privateRoot, privateOutputDirectory) {
		return frameSource{}, fmt.Errorf("isolated output %s is not a direct protected child", output.HostPath)
	}
	file, verify, err := platformOpenProtectedOutput(state.outputRoot, leaf)
	if err != nil {
		return frameSource{}, fmt.Errorf("open isolated output %s: %w", output.HostPath, err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > output.MaxBytes {
		return frameSource{}, errors.Join(fmt.Errorf("isolated output %s exceeded its bound", output.HostPath), err, verify(), file.Close())
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return frameSource{}, errors.Join(err, verify(), file.Close())
	}
	if _, err := file.Seek(0, 0); err != nil {
		return frameSource{}, errors.Join(err, verify(), file.Close())
	}
	if err := verify(); err != nil {
		return frameSource{}, errors.Join(fmt.Errorf("verify isolated output %s after hashing: %w", output.HostPath, err), file.Close())
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	return frameSource{
		description: frame{Name: output.HostPath, Bytes: info.Size(), SHA256: digest},
		reader:      file, close: func() error {
			return errors.Join(verify(), file.Close())
		},
		promote: func() (*platformPromotedInput, error) {
			return platformPromoteProtectedOutput(
				file, verify, info.Size(), digest, info.Mode().Perm(), output.HostPath,
			)
		},
	}, nil
}

func promotedArtifactName(semanticPath string) string {
	extension := filepath.Ext(filepath.Base(semanticPath))
	if len(extension) > 16 {
		return "artifact"
	}
	for _, character := range extension {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' {
			continue
		}
		return "artifact"
	}
	return "artifact" + extension
}

func bytesFrame(name string, content []byte) frameSource {
	return frameSource{
		description: frame{Name: name, Bytes: int64(len(content)), SHA256: hashBytes(content)},
		reader:      bytes.NewReader(content), close: func() error { return nil },
	}
}

func writeRejectedCommand(writer io.Writer, rejection error) error {
	frames := []frameSource{bytesFrame("stdout", nil), bytesFrame("stderr", nil)}
	header := response{Event: targetFinishedEvent, Error: rejection.Error(), ExitCode: -1, Frames: []frame{
		frames[0].description, frames[1].description,
	}}
	return errors.Join(
		writeJSONLine(writer, header),
		writeJSONLine(writer, response{Event: targetSettledEvent, ExitCode: -1}),
	)
}
