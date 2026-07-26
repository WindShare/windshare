package osfs

import (
	"errors"
	"fmt"
)

type outputV3ProbeDataProfile uint8

const (
	outputV3ProbeDataLinuxExt4 outputV3ProbeDataProfile = iota + 1
	outputV3ProbeDataWindowsNTFS
)

type outputV3ProbeObservedFile struct {
	present bool
	size    uint64
}

type outputV3ProbeCutObservation struct {
	stage       outputV3ProbeObservedFile
	anchor      outputV3ProbeObservedFile
	publication outputV3ProbeObservedFile
	record      outputV3ProbeObservedFile
	temporary   outputV3ProbeObservedFile
	candidate   bool
	installed   bool
}

func (observation *outputV3ProbeCutObservation) observeFile(name string, size uint64) error {
	if observation == nil {
		return errors.New("probe cut observation is absent")
	}
	observed := outputV3ProbeObservedFile{present: true, size: size}
	switch name {
	case "stage":
		observation.stage = observed
	case "anchor":
		observation.anchor = observed
	case "publication":
		observation.publication = observed
	case "record":
		observation.record = observed
	case "record.tmp":
		observation.temporary = observed
	default:
		return fmt.Errorf("unknown probe file %q", name)
	}
	return nil
}

func (observation *outputV3ProbeCutObservation) observeDirectory(name string) error {
	if observation == nil {
		return errors.New("probe cut observation is absent")
	}
	switch name {
	case "candidate":
		observation.candidate = true
	case "installed":
		observation.installed = true
	default:
		return fmt.Errorf("unknown probe directory %q", name)
	}
	return nil
}

type outputV3ProbeDataCut uint8

const (
	outputV3ProbeDataAbsent outputV3ProbeDataCut = iota + 1
	outputV3ProbeDataStage
	outputV3ProbeDataStageAnchor
	outputV3ProbeDataStageAnchorPublication
	outputV3ProbeDataAnchorPublication
	outputV3ProbeDataAnchor
)

type outputV3ProbeRecordCut uint8

const (
	outputV3ProbeRecordAbsent outputV3ProbeRecordCut = iota + 1
	outputV3ProbeRecordOld
	outputV3ProbeRecordTemporaryCreated
	outputV3ProbeRecordTemporaryReady
	outputV3ProbeRecordInstalled
)

type outputV3ProbeDirectoryCut uint8

const (
	outputV3ProbeDirectoriesAbsent outputV3ProbeDirectoryCut = iota + 1
	outputV3ProbeDirectoryCandidate
	outputV3ProbeDirectoryInstalled
	outputV3ProbeDirectoriesInstalledAndCandidate
)

func validateOutputV3ProbeCut(
	profile outputV3ProbeDataProfile,
	observation outputV3ProbeCutObservation,
) error {
	data, err := observation.dataCut(profile)
	if err != nil {
		return err
	}
	record, err := observation.recordCut()
	if err != nil {
		return err
	}
	directories := observation.directoryCut()

	// Records begin only after all three data links exist. Cleanup removes stage
	// first and anchor last, so stage-only and stage-plus-anchor cannot coexist
	// with a record artifact in any deterministic process-restart cut.
	if record != outputV3ProbeRecordAbsent &&
		(data == outputV3ProbeDataStage || data == outputV3ProbeDataStageAnchor) {
		return errors.New("probe record state is paired with an unreachable data-link prefix")
	}
	if directories == outputV3ProbeDirectoriesAbsent {
		return nil
	}
	if record != outputV3ProbeRecordAbsent && record != outputV3ProbeRecordInstalled {
		return errors.New("probe directory state precedes the installed record generation")
	}
	// Directory cleanup starts only after every data link and the installed
	// record have gone. Once the record is absent, any remaining data link makes
	// the directory observation ambiguous rather than a recoverable cleanup cut.
	if record == outputV3ProbeRecordAbsent && data != outputV3ProbeDataAbsent {
		return errors.New("probe directory state without a record retains an unreachable data link")
	}
	return nil
}

func (observation outputV3ProbeCutObservation) dataCut(
	profile outputV3ProbeDataProfile,
) (outputV3ProbeDataCut, error) {
	bits := uint8(0)
	if observation.stage.present {
		bits |= 1
	}
	if observation.anchor.present {
		bits |= 2
	}
	if observation.publication.present {
		bits |= 4
	}
	var cut outputV3ProbeDataCut
	switch bits {
	case 0:
		cut = outputV3ProbeDataAbsent
	case 1:
		cut = outputV3ProbeDataStage
	case 3:
		cut = outputV3ProbeDataStageAnchor
	case 7:
		cut = outputV3ProbeDataStageAnchorPublication
	case 6:
		cut = outputV3ProbeDataAnchorPublication
	case 2:
		cut = outputV3ProbeDataAnchor
	default:
		return 0, errors.New("probe data links do not form a reachable creation or cleanup cut")
	}

	switch profile {
	case outputV3ProbeDataLinuxExt4:
		if (observation.stage.present && observation.stage.size != 0) ||
			(observation.anchor.present && observation.anchor.size != 0) ||
			(observation.publication.present && observation.publication.size != 0) {
			return 0, errors.New("Linux probe data link has an invalid size")
		}
	case outputV3ProbeDataWindowsNTFS:
		if observation.stage.present && observation.stage.size > 1 {
			return 0, errors.New("Windows probe stage has an invalid size")
		}
		if (observation.anchor.present && observation.anchor.size != 1) ||
			(observation.publication.present && observation.publication.size != 1) {
			return 0, errors.New("Windows probe hard link has an invalid size")
		}
		if observation.stage.present && observation.stage.size == 0 && cut != outputV3ProbeDataStage {
			return 0, errors.New("Windows zero-length probe stage exists after hard-link creation")
		}
	default:
		return 0, errors.New("probe data profile is unsupported")
	}
	return cut, nil
}

func (observation outputV3ProbeCutObservation) recordCut() (outputV3ProbeRecordCut, error) {
	switch {
	case !observation.record.present && !observation.temporary.present:
		return outputV3ProbeRecordAbsent, nil
	case observation.record.present && observation.record.size == 0 && !observation.temporary.present:
		return outputV3ProbeRecordOld, nil
	case observation.record.present && observation.record.size == 0 &&
		observation.temporary.present && observation.temporary.size == 0:
		return outputV3ProbeRecordTemporaryCreated, nil
	case observation.record.present && observation.record.size == 0 &&
		observation.temporary.present && observation.temporary.size == 1:
		return outputV3ProbeRecordTemporaryReady, nil
	case observation.record.present && observation.record.size == 1 && !observation.temporary.present:
		return outputV3ProbeRecordInstalled, nil
	default:
		return 0, errors.New("probe records do not form a reachable replacement cut")
	}
}

func (observation outputV3ProbeCutObservation) directoryCut() outputV3ProbeDirectoryCut {
	switch {
	case observation.candidate && observation.installed:
		return outputV3ProbeDirectoriesInstalledAndCandidate
	case observation.candidate:
		return outputV3ProbeDirectoryCandidate
	case observation.installed:
		return outputV3ProbeDirectoryInstalled
	default:
		return outputV3ProbeDirectoriesAbsent
	}
}
