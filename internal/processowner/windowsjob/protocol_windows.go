package windowsjob

import (
	"fmt"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"github.com/windshare/windshare/internal/testrun"
	"github.com/windshare/windshare/internal/testtrace"
)

type launcherEvent struct {
	SchemaVersion string  `json:"schema_version"`
	Type          string  `json:"type"`
	PID           uint32  `json:"pid"`
	ProcessHandle uint64  `json:"process_handle"`
	InputHandle   uint64  `json:"input_handle"`
	SpawnFailure  *string `json:"spawn_failure"`
}

const (
	launcherEventRootStarted = "root-started"
	launcherEventSpawnFailed = "spawn-failed"
	launcherEventSchema      = "windshare.process-owner-launcher-event/v2"
)

func environmentStrings(
	environment []ownerprotocol.EnvironmentEntry,
	targetEventHandle uintptr,
	identity ownerprotocol.Identity,
) []string {
	result := make([]string, 0, len(environment)+4)
	for _, entry := range environment {
		result = append(result, entry.Name+"="+entry.Value)
	}
	result = append(result,
		testrun.RunIDEnvironment+"="+identity.RunID,
		testrun.OperationIDEnvironment+"="+identity.OperationID,
		testrun.ScenarioEnvironment+"="+identity.Scenario,
	)
	if targetEventHandle != 0 {
		result = append(result, testtrace.EventHandleEnvironment+"="+fmt.Sprint(targetEventHandle))
	}
	return result
}
