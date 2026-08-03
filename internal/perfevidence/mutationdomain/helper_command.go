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
	"time"

	"github.com/windshare/windshare/internal/perfevidence"
)

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
