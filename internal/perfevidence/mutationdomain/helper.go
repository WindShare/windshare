package mutationdomain

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
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
