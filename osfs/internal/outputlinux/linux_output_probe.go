//go:build linux

package outputlinux

import (
	"crypto/rand"
	"errors"
	"io"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/outputprobe"
)

// ProbeNamePrefix is used by process-restart integration fixtures.
const ProbeNamePrefix = outputprobe.NamePrefix

func (root *linuxOutputDirectory) probeRecoverableFeatures() error {
	return root.probeRecoverableFeaturesWithRandom(rand.Reader)
}

func (root *linuxOutputDirectory) destinationCapabilities() (outputcap.DestinationCapabilities, error) {
	return root.destinationCapabilitiesWithRandom(rand.Reader)
}

func (root *linuxOutputDirectory) destinationCapabilitiesWithRandom(random io.Reader) (outputcap.DestinationCapabilities, error) {
	results, err := root.probeDestinationCapabilitiesWithRandom(random)
	if err != nil {
		return outputcap.DestinationCapabilities{}, linuxError(err)
	}
	return results.Capabilities()
}

func (root *linuxOutputDirectory) probeRecoverableFeaturesWithRandom(random io.Reader) error {
	results, err := root.probeDestinationCapabilitiesWithRandom(random)
	return linuxProbeError(errors.Join(err, results.Err()))
}

func (root *linuxOutputDirectory) probeDestinationCapabilitiesWithRandom(
	random io.Reader,
) (results outputprobe.Results, resultErr error) {
	if err := root.verifyHandle(); err != nil {
		return results, err
	}
	lock, err := root.acquireOutputProbeLock()
	if err != nil {
		return results, err
	}
	defer func() {
		resultErr = linuxProbeError(errors.Join(resultErr, root.releaseOutputProbeLock(lock)))
	}()
	// A process-only root has no authority over a previous process's artifacts.
	if root.binding.restart != nil {
		if err := root.recoverOutputProbeLeftovers(); err != nil {
			return results, err
		}
	}
	workspace, err := outputprobe.Allocate(&linuxDirectory{native: root}, random)
	if err != nil {
		return results, err
	}
	results, probeErr := root.runCapabilityFacts(workspace)
	cleanupErr := workspace.Cleanup()
	if cleanupErr != nil {
		cleanupErr = errors.Join(outputcap.ErrUnsafeNamespace, cleanupErr)
	}
	return results, errors.Join(probeErr, cleanupErr)
}

func (root *linuxOutputDirectory) runCapabilityFacts(workspace *outputprobe.Workspace) (results outputprobe.Results, resultErr error) {
	if root.binding.restart == nil {
		results.SafePublish = workspace.SafePublish(outputprobe.LinuxExt4)
		if err := workspace.Clear(); err != nil {
			return results, errors.Join(outputcap.ErrUnsafeNamespace, err)
		}
		if results.SafePublish == nil {
			results.SafePublish = root.probePublicProfileStage(workspace)
			if err := workspace.Clear(); err != nil {
				return results, errors.Join(outputcap.ErrUnsafeNamespace, err)
			}
		}
		// Successful live I/O cannot manufacture a restart certificate.
		unsupported := linuxError(linuxUnsupported("probe output recovery", "output root has no restart certificate", nil))
		results.OperationRecovery, results.RangeRecovery, results.CrashCleanup = unsupported, unsupported, unsupported
		return results, nil
	}
	facts := []struct {
		probe func() error
		set   func(error)
	}{
		{func() error { return workspace.SafePublish(outputprobe.LinuxExt4) }, func(err error) { results.SafePublish = err }},
		{workspace.RangeRecovery, func(err error) { results.RangeRecovery = err }},
		{workspace.OperationRecovery, func(err error) { results.OperationRecovery = err }},
		{func() error { return root.probePublicProfileStage(workspace) }, func(err error) {
			results.SafePublish = errors.Join(results.SafePublish, err)
			results.CrashCleanup = err
		}},
	}
	for _, fact := range facts {
		fact.set(fact.probe())
		if err := workspace.Clear(); err != nil {
			return results, errors.Join(outputcap.ErrUnsafeNamespace, err)
		}
	}
	return results, nil
}

func (root *linuxOutputDirectory) probePublicProfileStage(workspace *outputprobe.Workspace) error {
	// Final-parent inheritance comes from O_TMPFILE; a private-directory create
	// would test different permissions from the file that the user receives.
	directory := &linuxDirectory{native: root}
	file, err := directory.CreateProcessStage(workspace.Directory(), outputprobe.PublicStageName, 0)
	err = errors.Join(err, workspace.KeepFile(outputprobe.PublicStageName, file))
	if err != nil {
		return linuxError(linuxUnsupported("probe public-profile stage", "anonymous stage installation is unavailable", err))
	}
	return workspace.VerifyFile(outputprobe.PublicStageName, false)
}

func linuxProbeError(err error) error {
	switch {
	case errors.Is(err, outputcap.ErrUnsafeNamespace):
		return errors.Join(errLinuxOutputUnsafe, err)
	case errors.Is(err, outputcap.ErrRecoverableOutputUnsupported):
		return errors.Join(errLinuxOutputUnsupported, err)
	default:
		return err
	}
}
