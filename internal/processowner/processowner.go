// Package processowner defines the small lifecycle contract shared by the test
// client and its platform-specific descendant supervisor.
package processowner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

const (
	MaximumDocumentBytes                = 1 << 20
	MaximumDeadlineMilliseconds         = 3_600_000
	MaximumTerminationGraceMilliseconds = 60_000
)

const (
	StatusStarted  = "started"
	StatusFinished = "finished"

	ReasonNatural     = "natural"
	ReasonInterrupt   = "interrupt"
	ReasonStop        = "stop"
	ReasonDeadline    = "deadline"
	ReasonParentLost  = "parent_lost"
	ReasonSpawnFailed = "spawn_failed"
)

const (
	ControlInterrupt byte = 'i'
	ControlStop      byte = 's'
)

// Config contains only inputs required to launch and retire a process tree.
// Correlation remains in the ordinary child environment and testrun events.
type Config struct {
	Executable                   string   `json:"executable"`
	Arguments                    []string `json:"arguments"`
	WorkingDirectory             string   `json:"working_directory"`
	Environment                  []string `json:"environment"`
	DeadlineMilliseconds         int64    `json:"deadline_milliseconds"`
	TerminationGraceMilliseconds int64    `json:"termination_grace_milliseconds"`
}

// Result reports the target's observable terminal state after descendant
// cleanup has completed. It is diagnostic state, not an authenticated proof.
type Result struct {
	ExitCode     *int64 `json:"exit_code,omitempty"`
	Signal       string `json:"signal,omitempty"`
	Reason       string `json:"reason"`
	Error        string `json:"error,omitempty"`
	CleanupError string `json:"cleanup_error,omitempty"`
}

type Status struct {
	State  string  `json:"state"`
	Result *Result `json:"result,omitempty"`
}

func ValidateConfig(config Config) error {
	if !filepath.IsAbs(config.Executable) || filepath.Clean(config.Executable) != config.Executable {
		return errors.New("process executable must be an absolute canonical path")
	}
	if !filepath.IsAbs(config.WorkingDirectory) || filepath.Clean(config.WorkingDirectory) != config.WorkingDirectory {
		return errors.New("process working directory must be an absolute canonical path")
	}
	if config.Arguments == nil || config.Environment == nil {
		return errors.New("process arguments and environment must be arrays")
	}
	if config.DeadlineMilliseconds < 1 || config.DeadlineMilliseconds > MaximumDeadlineMilliseconds {
		return fmt.Errorf("process deadline must be in [1, %d] milliseconds", MaximumDeadlineMilliseconds)
	}
	if config.TerminationGraceMilliseconds < 1 ||
		config.TerminationGraceMilliseconds > MaximumTerminationGraceMilliseconds {
		return fmt.Errorf(
			"process termination grace must be in [1, %d] milliseconds",
			MaximumTerminationGraceMilliseconds,
		)
	}
	for _, value := range append(append([]string(nil), config.Arguments...), config.Executable, config.WorkingDirectory) {
		if strings.IndexByte(value, 0) >= 0 {
			return errors.New("process command contains NUL")
		}
	}
	seen := make(map[string]struct{}, len(config.Environment))
	for _, entry := range config.Environment {
		name, _, found := strings.Cut(entry, "=")
		if !found || name == "" || strings.IndexByte(entry, 0) >= 0 {
			return errors.New("process environment contains an invalid entry")
		}
		key := strings.ToUpper(name)
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("process environment contains duplicate name %q", name)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func EncodeConfig(config Config) ([]byte, error) {
	if err := ValidateConfig(config); err != nil {
		return nil, err
	}
	return encodeDocument(config)
}

func DecodeConfig(reader io.Reader) (Config, error) {
	var config Config
	if err := decodeDocument(reader, &config); err != nil {
		return Config{}, err
	}
	if err := ValidateConfig(config); err != nil {
		return Config{}, err
	}
	return config, nil
}

func WriteStatus(writer io.Writer, status Status) error {
	if err := ValidateStatus(status); err != nil {
		return err
	}
	encoded, err := encodeDocument(status)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	for len(encoded) > 0 {
		written, writeErr := writer.Write(encoded)
		if writeErr != nil {
			return writeErr
		}
		if written < 1 || written > len(encoded) {
			return io.ErrShortWrite
		}
		encoded = encoded[written:]
	}
	return nil
}

func ValidateStatus(status Status) error {
	switch status.State {
	case StatusStarted:
		if status.Result != nil {
			return errors.New("started process status cannot contain a result")
		}
	case StatusFinished:
		if status.Result == nil {
			return errors.New("finished process status requires a result")
		}
		if err := validateResult(*status.Result); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported process status %q", status.State)
	}
	return nil
}

type StatusDecoder struct {
	decoder *json.Decoder
}

func NewStatusDecoder(reader io.Reader) *StatusDecoder {
	decoder := json.NewDecoder(io.LimitReader(reader, MaximumDocumentBytes*4))
	decoder.DisallowUnknownFields()
	return &StatusDecoder{decoder: decoder}
}

func (decoder *StatusDecoder) Next() (Status, error) {
	if decoder == nil || decoder.decoder == nil {
		return Status{}, errors.New("process status decoder is nil")
	}
	var status Status
	if err := decoder.decoder.Decode(&status); err != nil {
		return Status{}, err
	}
	if err := ValidateStatus(status); err != nil {
		return Status{}, err
	}
	return status, nil
}

func validateResult(result Result) error {
	switch result.Reason {
	case ReasonNatural, ReasonInterrupt, ReasonStop, ReasonDeadline, ReasonParentLost:
		if result.ExitCode == nil {
			return errors.New("started process result requires an exit code")
		}
	case ReasonSpawnFailed:
		if result.ExitCode != nil || result.Error == "" {
			return errors.New("spawn failure requires an error and excludes an exit code")
		}
	default:
		return fmt.Errorf("unsupported process termination reason %q", result.Reason)
	}
	for _, diagnostic := range []string{result.Signal, result.Error, result.CleanupError} {
		if strings.IndexByte(diagnostic, 0) >= 0 || len(diagnostic) > 4096 {
			return errors.New("process result diagnostic is invalid")
		}
	}
	return nil
}

func encodeDocument(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(encoded) == 0 || len(encoded) > MaximumDocumentBytes {
		return nil, fmt.Errorf("process document must contain at most %d bytes", MaximumDocumentBytes)
	}
	return encoded, nil
}

func decodeDocument(reader io.Reader, target any) error {
	encoded, err := io.ReadAll(io.LimitReader(reader, MaximumDocumentBytes+1))
	if err != nil {
		return err
	}
	if len(encoded) == 0 || len(encoded) > MaximumDocumentBytes {
		return fmt.Errorf("process document must contain at most %d bytes", MaximumDocumentBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("process document contains trailing data")
	}
	return nil
}
