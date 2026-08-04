package processowner

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Executable: filepath.Join(t.TempDir(), "target"), Arguments: []string{"one"},
		WorkingDirectory: t.TempDir(), Environment: []string{"A=one", "B="},
		DeadlineMilliseconds: 1000, TerminationGraceMilliseconds: 100,
	}
}

func TestConfigRoundTripAndValidation(t *testing.T) {
	config := validConfig(t)
	encoded, err := EncodeConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeConfig(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Executable != config.Executable || len(decoded.Environment) != 2 {
		t.Fatalf("decoded config = %+v", decoded)
	}

	invalid := config
	invalid.Environment = []string{"A=one", "a=two"}
	if err := ValidateConfig(invalid); err == nil {
		t.Fatal("duplicate environment was accepted")
	}
	if _, err := DecodeConfig(bytes.NewBufferString("{} trailing")); err == nil {
		t.Fatal("trailing config data was accepted")
	}
}

func TestConfigRejectsInvalidLifecycleInputs(t *testing.T) {
	tests := map[string]func(*Config){
		"relative executable": func(config *Config) { config.Executable = "target" },
		"unclean executable": func(config *Config) {
			config.Executable += string(os.PathSeparator) + ".." + string(os.PathSeparator) + "target"
		},
		"relative working directory": func(config *Config) { config.WorkingDirectory = "." },
		"nil arguments":              func(config *Config) { config.Arguments = nil },
		"nil environment":            func(config *Config) { config.Environment = nil },
		"zero deadline":              func(config *Config) { config.DeadlineMilliseconds = 0 },
		"excessive deadline": func(config *Config) {
			config.DeadlineMilliseconds = MaximumDeadlineMilliseconds + 1
		},
		"zero grace": func(config *Config) { config.TerminationGraceMilliseconds = 0 },
		"excessive grace": func(config *Config) {
			config.TerminationGraceMilliseconds = MaximumTerminationGraceMilliseconds + 1
		},
		"nul argument":               func(config *Config) { config.Arguments = []string{"bad\x00argument"} },
		"missing environment equals": func(config *Config) { config.Environment = []string{"A"} },
		"empty environment name":     func(config *Config) { config.Environment = []string{"=value"} },
		"nul environment":            func(config *Config) { config.Environment = []string{"A=bad\x00value"} },
		"duplicate environment":      func(config *Config) { config.Environment = []string{"A=one", "a=two"} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := validConfig(t)
			mutate(&config)
			if err := ValidateConfig(config); err == nil {
				t.Fatal("invalid config was accepted")
			}
			if _, err := EncodeConfig(config); err == nil {
				t.Fatal("invalid config was encoded")
			}
		})
	}
}

func TestConfigDecoderBoundsAndSchema(t *testing.T) {
	for name, document := range map[string]string{
		"empty":          "",
		"unknown field":  `{"unknown":true}`,
		"trailing value": `{} {}`,
		"oversized":      strings.Repeat("x", MaximumDocumentBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeConfig(strings.NewReader(document)); err == nil {
				t.Fatal("invalid document was accepted")
			}
		})
	}
	if _, err := DecodeConfig(failingReader{}); err == nil {
		t.Fatal("reader failure was ignored")
	}
}

func TestStatusStreamValidatesLifecycleShape(t *testing.T) {
	exitCode := int64(7)
	var stream bytes.Buffer
	for _, status := range []Status{
		{State: StatusStarted},
		{State: StatusFinished, Result: &Result{ExitCode: &exitCode, Reason: ReasonNatural}},
	} {
		if err := WriteStatus(&stream, status); err != nil {
			t.Fatal(err)
		}
	}
	decoder := NewStatusDecoder(&stream)
	started, err := decoder.Next()
	if err != nil || started.State != StatusStarted {
		t.Fatalf("started = %+v, %v", started, err)
	}
	finished, err := decoder.Next()
	if err != nil || finished.Result == nil || *finished.Result.ExitCode != exitCode {
		t.Fatalf("finished = %+v, %v", finished, err)
	}
	if _, err := decoder.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal stream error = %v", err)
	}
	if err := ValidateStatus(Status{State: StatusFinished}); err == nil {
		t.Fatal("finished status without result was accepted")
	}
}

func TestStatusRejectsInvalidStateAndResults(t *testing.T) {
	zero := int64(0)
	tests := map[string]Status{
		"started result": {State: StatusStarted, Result: &Result{ExitCode: &zero, Reason: ReasonNatural}},
		"unknown state":  {State: "waiting"},
		"missing result": {State: StatusFinished},
		"missing exit":   {State: StatusFinished, Result: &Result{Reason: ReasonNatural}},
		"unknown reason": {State: StatusFinished, Result: &Result{ExitCode: &zero, Reason: "unknown"}},
		"spawn with exit": {
			State: StatusFinished, Result: &Result{ExitCode: &zero, Reason: ReasonSpawnFailed, Error: "failed"},
		},
		"spawn without error": {State: StatusFinished, Result: &Result{Reason: ReasonSpawnFailed}},
		"nul diagnostic": {
			State: StatusFinished, Result: &Result{ExitCode: &zero, Reason: ReasonNatural, Error: "bad\x00error"},
		},
		"long diagnostic": {
			State:  StatusFinished,
			Result: &Result{ExitCode: &zero, Reason: ReasonNatural, CleanupError: strings.Repeat("x", 4097)},
		},
	}
	for name, status := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidateStatus(status); err == nil {
				t.Fatal("invalid status was accepted")
			}
			if err := WriteStatus(io.Discard, status); err == nil {
				t.Fatal("invalid status was written")
			}
		})
	}
	if _, err := (*StatusDecoder)(nil).Next(); err == nil {
		t.Fatal("nil decoder was accepted")
	}
	decoder := NewStatusDecoder(strings.NewReader(`{"state":"started","unknown":true}`))
	if _, err := decoder.Next(); err == nil {
		t.Fatal("unknown status field was accepted")
	}
}

func TestDocumentWritersPropagateFailures(t *testing.T) {
	if err := WriteStatus(shortWriter{}, Status{State: StatusStarted}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write error = %v", err)
	}
	writeFailure := errors.New("write failed")
	if err := WriteStatus(errorWriter{err: writeFailure}, Status{State: StatusStarted}); !errors.Is(err, writeFailure) {
		t.Fatalf("write error = %v", err)
	}
	if _, err := encodeDocument(make(chan int)); err == nil {
		t.Fatal("JSON marshal failure was ignored")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

type shortWriter struct{}

func (shortWriter) Write(value []byte) (int, error) { return len(value) - 1, nil }

type errorWriter struct{ err error }

func (writer errorWriter) Write([]byte) (int, error) { return 0, writer.err }
