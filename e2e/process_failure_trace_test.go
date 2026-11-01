package e2e

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
)

const retainedFailureTraceRecords = 64

// Child traces live in test-owned temporary directories. Preserve the causal
// tail before cleanup removes the only evidence of a hosted timing failure.
func (process *v2Process) logFailureTrace(t *testing.T) {
	t.Helper()
	if !t.Failed() {
		return
	}
	if err := process.close(); err != nil {
		t.Logf("%s diagnostic shutdown: %v", process.component, err)
	}
	file, err := os.Open(process.userTracePath)
	if err != nil {
		t.Logf("%s diagnostic trace: %v", process.component, err)
		return
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, maximumObservationRecordBytes)
	var tail [retainedFailureTraceRecords]string
	count := 0
	for scanner.Scan() {
		var record observationRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Logf("%s diagnostic trace decode: %v", process.component, err)
			continue
		}
		switch record.Event {
		case "protocol_operation", "protocol_response_send_returned",
			"sender_session_terminated", "receiver_termination",
			"lane_adopted", "lane_settlement", "relay_lifecycle",
			"transfer_lifecycle", "transfer_settled", "trace_summary":
			tail[count%len(tail)] = scanner.Text()
			count++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Logf("%s diagnostic trace read: %v", process.component, err)
	}
	for index := max(0, count-len(tail)); index < count; index++ {
		t.Logf("%s trace: %s", process.component, tail[index%len(tail)])
	}
}
