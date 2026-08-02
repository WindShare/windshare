//go:build linux

package testprocess

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/windshare/windshare/internal/processowner/protocol"
	"github.com/windshare/windshare/internal/testrun"
	"github.com/windshare/windshare/internal/testtrace"
)

const linuxEventSinkChildEnvironment = "WINDSHARE_LINUX_EVENT_SINK_CHILD"

func TestOpenLinuxEventSinkAdoptsConfiguredDescriptor(t *testing.T) {
	identity := protocol.Identity{RunID: "run", OperationID: "operation", Scenario: "scenario"}
	if os.Getenv(linuxEventSinkChildEnvironment) == "1" {
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
		return
	}
	t.Setenv(testrun.RunIDEnvironment, identity.RunID)
	t.Setenv(testrun.OperationIDEnvironment, identity.OperationID)
	t.Setenv(testrun.ScenarioEnvironment, identity.Scenario)
	t.Setenv(testtrace.EventFDEnvironment, "invalid")
	if _, err := OpenEventSink(identity); err == nil {
		t.Fatal("invalid event descriptor was accepted")
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	childEnvironment := eventSinkChildEnvironment(identity)
	child := exec.Command(executable, "-test.run=^TestOpenLinuxEventSinkAdoptsConfiguredDescriptor$", "-test.count=1")
	child.Env = childEnvironment
	// ExtraFiles defines fd 3 by contract; relying on an ambient descriptor number
	// made this capability test depend on unrelated runtime files and test order.
	child.ExtraFiles = []*os.File{writer}
	output, childErr := child.CombinedOutput()
	closeErr := writer.Close()
	if childErr != nil || closeErr != nil {
		t.Fatalf("run isolated event-sink fixture: child=%v close=%v output=%s", childErr, closeErr, output)
	}
	event, err := protocol.ReadLineDocument[protocol.Event](reader)
	if err != nil || event.Identity != identity {
		t.Fatalf("event = %#v, %v", event, err)
	}
	if _, err := OpenEventSink(protocol.Identity{}); err == nil {
		t.Fatal("invalid event identity was accepted")
	}
}

func eventSinkChildEnvironment(identity protocol.Identity) []string {
	overrides := []string{
		linuxEventSinkChildEnvironment + "=1",
		testrun.RunIDEnvironment + "=" + identity.RunID,
		testrun.OperationIDEnvironment + "=" + identity.OperationID,
		testrun.ScenarioEnvironment + "=" + identity.Scenario,
		testtrace.EventFDEnvironment + "=" + strconv.Itoa(3),
	}
	result := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		name, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		replaced := false
		for _, override := range overrides {
			overrideName, _, _ := strings.Cut(override, "=")
			if strings.EqualFold(name, overrideName) {
				replaced = true
				break
			}
		}
		if !replaced {
			result = append(result, entry)
		}
	}
	return append(result, overrides...)
}
