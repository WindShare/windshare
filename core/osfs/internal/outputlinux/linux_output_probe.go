//go:build linux

package outputlinux

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

func (root *linuxOutputDirectory) probeRecoverableFeaturesWithRandom(random io.Reader) (resultErr error) {
	const operation = "probe Linux output filesystem"
	if err := root.verifyHandle(); err != nil {
		return err
	}
	lock, err := root.acquireOutputProbeLock()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, root.releaseOutputProbeLock(lock)) }()
	if err := root.recoverOutputProbeLeftovers(); err != nil {
		return err
	}
	if random == nil {
		return linuxUnsafe(operation, "random source is absent", nil)
	}
	for range linuxOutputProbeAllocationAttempts {
		name, err := linuxNewOutputProbeName(random)
		if err != nil {
			return fmt.Errorf("%s: allocate private name: %w", operation, err)
		}
		directory, err := root.createPrivateDirectoryExact(name, linuxOutputDirectoryMode)
		if errors.Is(err, errLinuxOutputCollision) {
			continue
		}
		if err != nil {
			return err
		}
		probe := linuxOutputProbe{root: root, rootName: name, directory: directory}
		probeErr := probe.run()
		cleanupErr := probe.cleanup()
		if cleanupErr != nil {
			return linuxUnsafe(
				operation,
				"fixed probe namespace could not be removed without guessing",
				errors.Join(probeErr, cleanupErr),
			)
		}
		if probeErr != nil {
			if errors.Is(probeErr, errLinuxOutputUnsafe) {
				return probeErr
			}
			return linuxUnsupported(operation, "required ext4 feature probe failed", probeErr)
		}
		return nil
	}
	return linuxUnsafe(operation, "could not allocate a unique fixed probe namespace", nil)
}

type linuxOutputProbe struct {
	root                   *linuxOutputDirectory
	rootName               string
	directory              *linuxOutputDirectory
	stage                  *linuxOutputRegularFile
	anchor                 *linuxOutputRegularFile
	publication            *linuxOutputRegularFile
	oldRecord              *linuxOutputRegularFile
	newRecord              *linuxOutputRegularFile
	installedDirectory     *linuxOutputDirectory
	collisionDirectory     *linuxOutputDirectory
	stagePresent           bool
	anchorPresent          bool
	publicationPresent     bool
	oldRecordPresent       bool
	temporaryRecordPresent bool
	newRecordPresent       bool
	installedPresent       bool
	collisionPresent       bool
}

func (probe *linuxOutputProbe) run() error {
	const operation = "exercise Linux output filesystem features"
	stage, err := probe.directory.createRegularFileExact("stage", linuxOutputStateFileMode, 0)
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
	anchor, err := probe.directory.openRegularFileExact("anchor", false, linuxOutputStateFileMode)
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
	publication, err := probe.directory.openRegularFileExact("publication", false, linuxOutputStateFileMode)
	if err != nil {
		return err
	}
	probe.publication = publication
	if err := probe.directory.linkRegularFileNoReplace(probe.directory, "anchor", anchor, "publication"); !errors.Is(err, errLinuxOutputCollision) {
		return linuxUnsupported(operation, "hard-link publication is not atomic no-replace", err)
	}

	oldRecord, err := probe.directory.createRegularFileExact("record", linuxOutputStateFileMode, 0)
	if err != nil {
		return err
	}
	probe.oldRecord = oldRecord
	probe.oldRecordPresent = true
	newRecord, err := probe.directory.createRegularFileExact("record.tmp", linuxOutputStateFileMode, 1)
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

func (probe *linuxOutputProbe) cleanup() error {
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
