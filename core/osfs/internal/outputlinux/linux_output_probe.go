//go:build linux

package outputlinux

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

const (
	linuxOutputProbePrefix             = ".windshare-output.probe-"
	linuxOutputProbeRandomBytes        = 16
	linuxOutputProbeAllocationAttempts = 16
)

// ProbeNamePrefix lets process-restart integration tests construct the exact
// reserved leftover that Open must recover before starting a new probe.
const ProbeNamePrefix = linuxOutputProbePrefix

func (root *linuxOutputDirectory) probeRecoverableFeatures() error {
	return root.probeRecoverableFeaturesWithRandom(rand.Reader)
}

func (root *linuxOutputDirectory) destinationCapabilities() (outputcap.DestinationCapabilities, error) {
	return root.destinationCapabilitiesWithRandom(rand.Reader)
}

func (root *linuxOutputDirectory) destinationCapabilitiesWithRandom(
	random io.Reader,
) (outputcap.DestinationCapabilities, error) {
	results, err := root.probeDestinationCapabilitiesWithRandom(random)
	if err != nil {
		return outputcap.DestinationCapabilities{}, err
	}
	return linuxDestinationCapabilitiesFromResults(results)
}

type linuxCapabilityProbeResults struct {
	safePublish       error
	operationRecovery error
	rangeRecovery     error
	crashCleanup      error
}

func linuxDestinationCapabilitiesFromResults(
	results linuxCapabilityProbeResults,
) (outputcap.DestinationCapabilities, error) {
	facts := []struct {
		err    error
		reason outputcap.CapabilityReason
	}{
		{results.safePublish, outputcap.CapabilityReasonUnsafePublication},
		{results.operationRecovery, outputcap.CapabilityReasonUnverifiableOperationRecovery},
		{results.rangeRecovery, outputcap.CapabilityReasonUnverifiableRangeRecovery},
		{results.crashCleanup, outputcap.CapabilityReasonUnverifiableCrashCleanup},
	}
	evidence := make([]outputcap.CapabilityEvidence, len(facts))
	for index, fact := range facts {
		if fact.err == nil {
			evidence[index] = outputcap.SupportedCapability()
			continue
		}
		if errors.Is(fact.err, errLinuxOutputUnsafe) {
			return outputcap.DestinationCapabilities{}, linuxV3Error(fact.err)
		}
		evidence[index], _ = outputcap.UnsupportedCapability(fact.reason)
	}
	capabilities, err := outputcap.NewDestinationCapabilities(
		evidence[0], evidence[1], evidence[2], evidence[3])
	// Unsupported is a valid fact, not a probe transport failure. Callers reduce
	// the complete report into resumable/live-only/pre-content rejection.
	return capabilities, err
}

func (root *linuxOutputDirectory) probeRecoverableFeaturesWithRandom(random io.Reader) error {
	results, err := root.probeDestinationCapabilitiesWithRandom(random)
	if err != nil {
		return err
	}
	for _, factErr := range []error{
		results.safePublish,
		results.operationRecovery,
		results.rangeRecovery,
		results.crashCleanup,
	} {
		if factErr != nil {
			return factErr
		}
	}
	return nil
}

func (root *linuxOutputDirectory) probeDestinationCapabilitiesWithRandom(
	random io.Reader,
) (results linuxCapabilityProbeResults, resultErr error) {
	const operation = "probe Linux output filesystem"
	if err := root.verifyHandle(); err != nil {
		return results, err
	}
	lock, err := root.acquireOutputProbeLock()
	if err != nil {
		return results, err
	}
	defer func() {
		if releaseErr := root.releaseOutputProbeLock(lock); releaseErr != nil {
			resultErr = errors.Join(resultErr, releaseErr)
		}
	}()
	if err := root.recoverOutputProbeLeftovers(); err != nil {
		return results, err
	}
	if random == nil {
		return results, linuxUnsafe(operation, "random source is absent", nil)
	}
	for range linuxOutputProbeAllocationAttempts {
		name, err := linuxNewOutputProbeName(random)
		if err != nil {
			return results, fmt.Errorf("%s: allocate private name: %w", operation, err)
		}
		directory, err := root.createPrivateDirectoryExact(name, linuxOutputDirectoryMode)
		if errors.Is(err, errLinuxOutputCollision) {
			continue
		}
		if err != nil {
			return results, err
		}
		probe := linuxOutputProbe{root: root, rootName: name, directory: directory}
		results, probeErr := probe.runCapabilityFacts()
		cleanupErr := probe.cleanup()
		if probeErr != nil {
			return results, probeErr
		}
		if cleanupErr != nil {
			return results, linuxUnsafe(
				operation,
				"fixed probe namespace could not be removed without guessing",
				cleanupErr,
			)
		}
		return results, nil
	}
	return results, linuxUnsafe(operation, "could not allocate a unique fixed probe namespace", nil)
}

type linuxOutputProbe struct {
	root                   *linuxOutputDirectory
	rootName               string
	directory              *linuxOutputDirectory
	stage                  *linuxOutputRegularFile
	liveStage              *linuxOutputRegularFile
	anchor                 *linuxOutputRegularFile
	publication            *linuxOutputRegularFile
	oldRecord              *linuxOutputRegularFile
	newRecord              *linuxOutputRegularFile
	installedDirectory     *linuxOutputDirectory
	collisionDirectory     *linuxOutputDirectory
	stagePresent           bool
	liveStagePresent       bool
	anchorPresent          bool
	publicationPresent     bool
	oldRecordPresent       bool
	temporaryRecordPresent bool
	newRecordPresent       bool
	installedPresent       bool
	collisionPresent       bool
}

func (probe *linuxOutputProbe) runCapabilityFacts() (results linuxCapabilityProbeResults, resultErr error) {
	// Each report is native evidence for one semantic fact. Later runtime
	// composition decides which conjunction selects resumable or live-only mode;
	// the platform must not erase unrelated evidence through probe ordering.
	facts := []struct {
		probe func() error
		set   func(error)
	}{
		{probe.probeSafePublish, func(err error) { results.safePublish = err }},
		{probe.probeRangeRecovery, func(err error) { results.rangeRecovery = err }},
		{probe.probeOperationRecovery, func(err error) { results.operationRecovery = err }},
		{probe.probeCrashCleanup, func(err error) { results.crashCleanup = err }},
	}
	for _, fact := range facts {
		fact.set(fact.probe())
		if err := probe.cleanupFactArtifacts(); err != nil {
			return results, linuxUnsafe(
				"probe Linux destination capability",
				"one fact could not clean its exact native artifacts before the next fact",
				err,
			)
		}
	}
	return results, nil
}

func (probe *linuxOutputProbe) probeSafePublish() error {
	const operation = "probe Linux safe publication"
	stage, err := probe.directory.createPrivateRegularFileExact("stage", linuxOutputStateFileMode, 0)
	if err != nil {
		return err
	}
	probe.stage = stage
	probe.stagePresent = true
	if err := probe.directory.linkRegularFileNoReplace(probe.directory, "stage", stage, "anchor"); err != nil {
		return err
	}
	probe.anchorPresent = true
	// The source handle is sufficient to clean a successfully linked entry if
	// reopening the new name is the operation that fails.
	probe.anchor = stage
	anchor, err := probe.directory.openRegularFile("anchor", false)
	if err != nil {
		return err
	}
	probe.anchor = anchor
	same, err := linuxSameOpenRegularFile(stage, anchor)
	if err != nil || !same {
		return errors.Join(linuxUnsafe(operation, "hard-link same-object verification failed", nil), err)
	}
	if err := probe.directory.linkRegularFileNoReplace(probe.directory, "anchor", anchor, "publication"); err != nil {
		return err
	}
	probe.publicationPresent = true
	probe.publication = anchor
	publication, err := probe.directory.openRegularFile("publication", false)
	if err != nil {
		return err
	}
	probe.publication = publication
	if err := probe.directory.linkRegularFileNoReplace(probe.directory, "anchor", anchor, "publication"); !errors.Is(err, errLinuxOutputCollision) {
		return linuxUnsupported(operation, "hard-link publication is not atomic no-replace", err)
	}
	return nil
}

func (probe *linuxOutputProbe) probeRangeRecovery() error {
	const operation = "probe Linux range recovery"
	oldRecord, err := probe.directory.createPrivateRegularFileExact("record", linuxOutputStateFileMode, 0)
	if err != nil {
		return err
	}
	probe.oldRecord = oldRecord
	probe.oldRecordPresent = true
	newRecord, err := probe.directory.createPrivateRegularFileExact("record.tmp", linuxOutputStateFileMode, 1)
	if err != nil {
		return err
	}
	probe.newRecord = newRecord
	probe.temporaryRecordPresent = true
	if err := probe.directory.renameRegularFile(
		"record.tmp",
		newRecord,
		probe.directory,
		"record",
		linuxRenameReplace,
	); err != nil {
		return err
	}
	probe.temporaryRecordPresent = false
	probe.oldRecordPresent = false
	probe.newRecordPresent = true
	reopened, err := probe.directory.openRegularFileExact("record", false, linuxOutputStateFileMode)
	if err != nil {
		return err
	}
	same, compareErr := linuxSameOpenRegularFile(newRecord, reopened)
	closeErr := reopened.close()
	if compareErr != nil || closeErr != nil || !same {
		return errors.Join(
			linuxUnsafe(operation, "atomic record replacement did not install the expected object", nil),
			compareErr,
			closeErr,
		)
	}
	return nil
}

func (probe *linuxOutputProbe) probeOperationRecovery() error {
	const operation = "probe Linux operation recovery"
	candidate, err := probe.directory.createPrivateDirectoryExact("candidate", linuxOutputDirectoryMode)
	if err != nil {
		return err
	}
	probe.installedDirectory = candidate
	if err := probe.directory.renameDirectory(
		"candidate",
		candidate,
		probe.directory,
		"installed",
		linuxRenameNoReplace,
	); err != nil {
		return err
	}
	probe.installedPresent = true
	collisionCandidate, err := probe.directory.createPrivateDirectoryExact("candidate", linuxOutputDirectoryMode)
	if err != nil {
		return err
	}
	probe.collisionDirectory = collisionCandidate
	probe.collisionPresent = true
	if err := probe.directory.renameDirectory(
		"candidate",
		collisionCandidate,
		probe.directory,
		"installed",
		linuxRenameNoReplace,
	); !errors.Is(err, errLinuxOutputCollision) {
		return linuxUnsupported(operation, "directory installation is not atomic no-replace", err)
	}
	return nil
}

func (probe *linuxOutputProbe) probeCrashCleanup() error {
	const operation = "probe Linux crash cleanup"
	if err := probe.directory.validatePrivateAuthority(operation); err != nil {
		return err
	}
	// Exercise the exact live-only primitive independently of ordinary publish:
	// unprivileged O_TMPFILE through the public parent, then AT_EMPTY_PATH install
	// into the protected proof directory. Unsupported kernels/filesystems disable
	// only CrashCleanup; a named-file or ACL-copy fallback is forbidden.
	liveStage, err := probe.root.createLiveCleanupStage(probe.directory, "live-stage", 0)
	if err != nil {
		return linuxUnsupported(operation, "anonymous public-profile stage installation is unavailable", err)
	}
	probe.liveStage = liveStage
	probe.liveStagePresent = true
	matches, err := probe.directory.regularEntryMatches("live-stage", liveStage)
	if err != nil || !matches {
		return errors.Join(linuxUnsafe(operation,
			"protected cleanup name does not identify its anonymous stage", nil), err)
	}
	return nil
}

func (probe *linuxOutputProbe) cleanupFactArtifacts() error {
	var result error
	removeFile := func(present *bool, name string, file **linuxOutputRegularFile) {
		if *file == nil {
			*present = false
			return
		}
		if *present {
			if err := probe.directory.unlinkRegularFile(name, *file); err != nil {
				result = errors.Join(result, err)
				return
			}
			*present = false
		}
		result = errors.Join(result, (*file).close())
		*file = nil
	}
	removeDirectory := func(present *bool, name string, directory **linuxOutputDirectory) {
		if *directory == nil {
			*present = false
			return
		}
		if *present {
			if err := probe.directory.unlinkDirectory(name, *directory); err != nil {
				result = errors.Join(result, err)
				return
			}
			*present = false
		}
		result = errors.Join(result, (*directory).close())
		*directory = nil
	}

	removeFile(&probe.liveStagePresent, "live-stage", &probe.liveStage)
	removeFile(&probe.stagePresent, "stage", &probe.stage)
	removeFile(&probe.publicationPresent, "publication", &probe.publication)
	removeFile(&probe.anchorPresent, "anchor", &probe.anchor)
	if probe.temporaryRecordPresent {
		removeFile(&probe.temporaryRecordPresent, "record.tmp", &probe.newRecord)
	} else if probe.newRecordPresent {
		removeFile(&probe.newRecordPresent, "record", &probe.newRecord)
	}
	removeFile(&probe.oldRecordPresent, "record", &probe.oldRecord)
	removeDirectory(&probe.collisionPresent, "candidate", &probe.collisionDirectory)
	removeDirectory(&probe.installedPresent, "installed", &probe.installedDirectory)
	return result
}

func (probe *linuxOutputProbe) cleanup() error {
	if probe.liveStagePresent {
		if err := probe.directory.unlinkRegularFile("live-stage", probe.liveStage); err != nil {
			return errors.Join(err, probe.closeHandles())
		}
		probe.liveStagePresent = false
	}
	if probe.stagePresent {
		if err := probe.directory.unlinkRegularFile("stage", probe.stage); err != nil {
			return errors.Join(err, probe.closeHandles())
		}
		probe.stagePresent = false
	}
	if probe.publicationPresent {
		if err := probe.directory.unlinkRegularFile("publication", probe.publication); err != nil {
			return errors.Join(err, probe.closeHandles())
		}
		probe.publicationPresent = false
	}
	if probe.anchorPresent {
		if err := probe.directory.unlinkRegularFile("anchor", probe.anchor); err != nil {
			return errors.Join(err, probe.closeHandles())
		}
		probe.anchorPresent = false
	}
	if probe.temporaryRecordPresent {
		if err := probe.directory.unlinkRegularFile("record.tmp", probe.newRecord); err != nil {
			return errors.Join(err, probe.closeHandles())
		}
		probe.temporaryRecordPresent = false
	}
	if probe.newRecordPresent {
		if err := probe.directory.unlinkRegularFile("record", probe.newRecord); err != nil {
			return errors.Join(err, probe.closeHandles())
		}
		probe.newRecordPresent = false
	}
	if probe.oldRecordPresent {
		if err := probe.directory.unlinkRegularFile("record", probe.oldRecord); err != nil {
			return errors.Join(err, probe.closeHandles())
		}
		probe.oldRecordPresent = false
	}
	if probe.collisionPresent {
		if err := probe.directory.unlinkDirectory("candidate", probe.collisionDirectory); err != nil {
			return errors.Join(err, probe.closeHandles())
		}
		probe.collisionPresent = false
	}
	if probe.installedPresent {
		if err := probe.directory.unlinkDirectory("installed", probe.installedDirectory); err != nil {
			return errors.Join(err, probe.closeHandles())
		}
		probe.installedPresent = false
	}
	closeErr := probe.closeHandles()
	removeErr := probe.root.unlinkDirectory(probe.rootName, probe.directory)
	directoryCloseErr := probe.directory.close()
	return errors.Join(closeErr, removeErr, directoryCloseErr)
}

func (probe *linuxOutputProbe) closeHandles() error {
	return errors.Join(
		probe.stage.close(),
		probe.liveStage.close(),
		probe.anchor.close(),
		probe.publication.close(),
		probe.oldRecord.close(),
		probe.newRecord.close(),
		probe.installedDirectory.close(),
		probe.collisionDirectory.close(),
	)
}

func linuxNewOutputProbeName(random io.Reader) (string, error) {
	var entropy [linuxOutputProbeRandomBytes]byte
	if _, err := io.ReadFull(random, entropy[:]); err != nil {
		return "", err
	}
	return linuxOutputProbePrefix + hex.EncodeToString(entropy[:]), nil
}
