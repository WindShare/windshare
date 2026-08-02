//go:build windows

package testprocess

import (
	"os"
	"strconv"
	"testing"

	"github.com/windshare/windshare/internal/processowner/protocol"
	"github.com/windshare/windshare/internal/testrun"
	"github.com/windshare/windshare/internal/testtrace"
	"golang.org/x/sys/windows"
)

func TestOpenWindowsEventSinkAdoptsConfiguredHandle(t *testing.T) {
	identity := protocol.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	t.Setenv(testrun.RunIDEnvironment, identity.RunID)
	t.Setenv(testrun.OperationIDEnvironment, identity.OperationID)
	t.Setenv(testrun.ScenarioEnvironment, identity.Scenario)
	t.Setenv(testtrace.EventHandleEnvironment, "invalid")
	if _, err := OpenEventSink(identity); err == nil {
		t.Fatal("invalid event handle was accepted")
	}
	var readerHandle windows.Handle
	var writerHandle windows.Handle
	if err := windows.CreatePipe(&readerHandle, &writerHandle, nil, 0); err != nil {
		t.Fatal(err)
	}
	reader := os.NewFile(uintptr(readerHandle), "test-event-reader")
	if reader == nil {
		_ = windows.CloseHandle(readerHandle)
		_ = windows.CloseHandle(writerHandle)
		t.Fatal("adopt test-event reader")
	}
	defer reader.Close()
	t.Setenv(testtrace.EventHandleEnvironment, strconv.FormatUint(uint64(writerHandle), 10))
	sink, err := OpenEventSink(identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Emit("fixture", "ready", "succeeded", nil); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	event, err := protocol.ReadLineDocument[protocol.Event](reader)
	if err != nil || event.Identity != identity {
		t.Fatalf("event = %#v, %v", event, err)
	}
	if _, err := OpenEventSink(protocol.Identity{}); err == nil {
		t.Fatal("invalid event identity was accepted")
	}
}
