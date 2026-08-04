package perfevidence

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"sync"
)

const (
	defaultSampleCount        = 1
	maximumCLIDiagnosticBytes = 2 << 10
)

type JSONLogger struct {
	Writer io.Writer
	mu     sync.Mutex
}

func (logger *JSONLogger) Log(event Event) error {
	if logger == nil || logger.Writer == nil {
		return nil
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode structured event: %w", err)
	}
	logger.mu.Lock()
	defer logger.mu.Unlock()
	if _, err := fmt.Fprintln(logger.Writer, string(encoded)); err != nil {
		return fmt.Errorf("write structured event: %w", err)
	}
	return nil
}

func Main(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("perfevidence", flag.ContinueOnError)
	flags.SetOutput(stderr)
	repository := flags.String("repository", "", "repository root; empty searches parent directories")
	samples := flags.Int("samples", defaultSampleCount, "independent go test processes per workload")
	workloadList := flags.String("workloads", "", "comma-separated workload IDs; empty selects all")
	list := flags.Bool("list", false, "list workload IDs without running them")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeCLIDiagnostic(stderr, "perfevidence does not accept positional arguments")
		return 2
	}
	if *list {
		return writeWorkloadList(stdout, stderr)
	}
	application := Application{
		Commands: ExecRunner{},
		Logger:   &JSONLogger{Writer: stderr},
	}
	report, runErr := application.Run(ctx, RunConfig{
		RepositoryRoot: *repository,
		SampleCount:    *samples,
		WorkloadIDs:    splitList(*workloadList),
	})
	if report.RunID != "" {
		if err := encodeReport(stdout, report); err != nil {
			writeCLIDiagnostic(stderr, "write performance report: "+err.Error())
			return 1
		}
	}
	if runErr != nil {
		writeCLIDiagnostic(stderr, "performance runner failed: "+runErr.Error())
		return 1
	}
	return 0
}

func writeWorkloadList(stdout, stderr io.Writer) int {
	for _, id := range WorkloadIDs() {
		if _, err := fmt.Fprintln(stdout, id); err != nil {
			writeCLIDiagnostic(stderr, "write workload list: "+err.Error())
			return 1
		}
	}
	return 0
}

func encodeReport(writer io.Writer, report Report) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("encode performance report: %w", err)
	}
	return nil
}

func writeCLIDiagnostic(writer io.Writer, message string) {
	if len(message) > maximumCLIDiagnosticBytes {
		message = message[:maximumCLIDiagnosticBytes]
	}
	_, _ = fmt.Fprintln(writer, message)
}

func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
