package outputportable

import (
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/outputprobe"
)

func (platform *Platform) destinationCapabilities(
	probe func(outputcap.Directory) (outputcap.DestinationCapabilities, error),
) (outputcap.DestinationCapabilities, error) {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	if err := platform.root.validate(); err != nil {
		return outputcap.DestinationCapabilities{}, err
	}
	if platform.capabilities.Valid() {
		return platform.capabilities, nil
	}
	capabilities, err := probe(platform.root)
	// Capabilities belong to this opened destination. A transient I/O failure
	// must remain retryable instead of becoming a cached unsupported verdict.
	if err == nil {
		platform.capabilities = capabilities
	}
	return capabilities, err
}

func probeDestinationCapabilities(root outputcap.Directory) (capabilities outputcap.DestinationCapabilities, resultErr error) {
	workspace, err := outputprobe.Allocate(root, rand.Reader)
	if err != nil {
		return capabilities, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, workspace.Cleanup())
		if resultErr != nil {
			capabilities = outputcap.DestinationCapabilities{}
		}
	}()
	results := outputprobe.Results{CrashCleanup: outputcap.ErrRecoverableOutputUnsupported}
	for _, fact := range []struct {
		name   string
		probe  func() error
		result *error
	}{
		{"file publication", workspace.FilePublication, &results.SafePublish},
		{"directory installation", workspace.OperationRecovery, &results.OperationRecovery},
	} {
		*fact.result = fact.probe()
		cleanupErr := workspace.Clear()
		if cleanupErr != nil {
			return capabilities, fmt.Errorf("probe portable %s: %w", fact.name, errors.Join(*fact.result, cleanupErr))
		}
		if *fact.result != nil &&
			!errors.Is(*fact.result, outputcap.ErrOrdinaryOutputUnsupported) &&
			!errors.Is(*fact.result, outputcap.ErrRecoverableOutputUnsupported) {
			return capabilities, fmt.Errorf("probe portable %s: %w", fact.name, *fact.result)
		}
		if results.SafePublish != nil {
			// Both task records and range checkpoints need the same no-replace
			// installation as public files; none can bypass a missing primitive.
			results.OperationRecovery, results.RangeRecovery = results.SafePublish, results.SafePublish
			break
		}
	}
	// These probes establish usable primitives, not power-loss durability or
	// strict restart identity. Portable recovery retains its best-effort model.
	return results.Capabilities()
}
