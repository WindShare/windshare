package main

type launcherEvent struct {
	SchemaVersion int     `json:"schemaVersion"`
	Type          string  `json:"type"`
	PID           uint32  `json:"pid"`
	ProcessHandle uint64  `json:"processHandle"`
	SpawnFailure  *string `json:"spawnFailure"`
}

const (
	launcherEventRootStarted = "root-started"
	launcherEventSpawnFailed = "spawn-failed"
)

func environmentStrings(environment []environmentEntry) []string {
	result := make([]string, len(environment))
	for index, entry := range environment {
		result[index] = entry.Name + "=" + entry.Value
	}
	return result
}

func ensureFreshControlDestination(path string) error {
	return ensureFreshPrivateDestination(path, "control")
}
