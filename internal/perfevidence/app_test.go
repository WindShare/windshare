package perfevidence

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestMainListsEveryMaintainedWorkload(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := Main(context.Background(), []string{"-list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	want := strings.Join(WorkloadIDs(), "\n") + "\n"
	if stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func TestMainRejectsArgumentsBeforeRunningBenchmarks(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
		wantCode  int
		wantError string
	}{
		{name: "positional", arguments: []string{"unexpected"}, wantCode: 2, wantError: "does not accept positional"},
		{name: "unknown workload", arguments: []string{"-workloads", "missing"}, wantCode: 1, wantError: "unknown performance workload"},
		{name: "invalid samples", arguments: []string{"-samples", "0"}, wantCode: 1, wantError: "sample count must be positive"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := Main(context.Background(), test.arguments, &stdout, &stderr)
			if code != test.wantCode || !strings.Contains(stderr.String(), test.wantError) || stdout.Len() != 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestJSONLoggerWritesOneStructuredEventPerLine(t *testing.T) {
	var output bytes.Buffer
	logger := &JSONLogger{Writer: &output}
	want := Event{
		Timestamp: time.Unix(1, 0).UTC(), RunID: "run", OperationID: "ready-scaling-sample-01",
		Scenario: "ready-scaling", Component: performanceComponent,
		Milestone: "benchmark-finished", Outcome: "succeeded", WorkloadID: "ready-scaling", SampleIndex: 1,
	}
	if err := logger.Log(want); err != nil {
		t.Fatal(err)
	}
	var got Event
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &got); err != nil {
		t.Fatalf("event is not JSON: %v: %s", err, output.String())
	}
	if got != want {
		t.Fatalf("event = %+v, want %+v", got, want)
	}
}

func TestSplitListIgnoresWhitespaceWithoutReorderingSelection(t *testing.T) {
	got := splitList(" ready-scaling, ,relay-registration-wire ")
	if strings.Join(got, ",") != "ready-scaling,relay-registration-wire" {
		t.Fatalf("selection = %q", got)
	}
}
