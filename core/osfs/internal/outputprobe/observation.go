// Package outputprobe validates native feature-probe leftovers against the
// deterministic process-restart cuts that the certified filesystems can
// produce.
package outputprobe

import (
	"errors"
	"fmt"
)

// Profile selects the native data-link shape used by a certified output probe.
type Profile uint8

const (
	// LinuxExt4 validates the zero-length link shape used by the ext4 probe.
	LinuxExt4 Profile = iota + 1
	// WindowsNTFS validates the one-byte hard-link shape used by the NTFS probe.
	WindowsNTFS
)

type observedFile struct {
	present bool
	size    uint64
}

// Observation accumulates the fixed probe vocabulary without granting path
// authority. Native recovery retains the opened handles separately and submits
// only immutable presence and size facts here.
type Observation struct {
	stage       observedFile
	anchor      observedFile
	publication observedFile
	record      observedFile
	temporary   observedFile
	candidate   bool
	installed   bool
}

// ObserveFile records one recognized probe file.
func (observation *Observation) ObserveFile(name string, size uint64) error {
	if observation == nil {
		return errors.New("probe cut observation is absent")
	}
	observed := observedFile{present: true, size: size}
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

// ObserveDirectory records one recognized probe directory.
func (observation *Observation) ObserveDirectory(name string) error {
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

type dataCut uint8

const (
	dataAbsent dataCut = iota + 1
	dataStage
	dataStageAnchor
	dataStageAnchorPublication
	dataAnchorPublication
	dataAnchor
)

type recordCut uint8

const (
	recordAbsent recordCut = iota + 1
	recordOld
	recordTemporaryCreated
	recordTemporaryReady
	recordInstalled
)

type directoryCut uint8

const (
	directoriesAbsent directoryCut = iota + 1
	directoryCandidate
	directoryInstalled
	directoriesInstalledAndCandidate
)

// Validate rejects observations that cannot occur at a deterministic
// process-restart cut for profile.
func Validate(profile Profile, observation Observation) error {
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
	if record != recordAbsent && (data == dataStage || data == dataStageAnchor) {
		return errors.New("probe record state is paired with an unreachable data-link prefix")
	}
	if directories == directoriesAbsent {
		return nil
	}
	if record != recordAbsent && record != recordInstalled {
		return errors.New("probe directory state precedes the installed record generation")
	}
	// Directory cleanup starts only after every data link and the installed
	// record have gone. Once the record is absent, any remaining data link makes
	// the directory observation ambiguous rather than a recoverable cleanup cut.
	if record == recordAbsent && data != dataAbsent {
		return errors.New("probe directory state without a record retains an unreachable data link")
	}
	return nil
}

func (observation Observation) dataCut(profile Profile) (dataCut, error) {
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
	var cut dataCut
	switch bits {
	case 0:
		cut = dataAbsent
	case 1:
		cut = dataStage
	case 3:
		cut = dataStageAnchor
	case 7:
		cut = dataStageAnchorPublication
	case 6:
		cut = dataAnchorPublication
	case 2:
		cut = dataAnchor
	default:
		return 0, errors.New("probe data links do not form a reachable creation or cleanup cut")
	}

	switch profile {
	case LinuxExt4:
		if (observation.stage.present && observation.stage.size != 0) ||
			(observation.anchor.present && observation.anchor.size != 0) ||
			(observation.publication.present && observation.publication.size != 0) {
			return 0, errors.New("linux probe data link has an invalid size")
		}
	case WindowsNTFS:
		if observation.stage.present && observation.stage.size > 1 {
			return 0, errors.New("windows probe stage has an invalid size")
		}
		if (observation.anchor.present && observation.anchor.size != 1) ||
			(observation.publication.present && observation.publication.size != 1) {
			return 0, errors.New("windows probe hard link has an invalid size")
		}
		if observation.stage.present && observation.stage.size == 0 && cut != dataStage {
			return 0, errors.New("windows zero-length probe stage exists after hard-link creation")
		}
	default:
		return 0, errors.New("probe data profile is unsupported")
	}
	return cut, nil
}

func (observation Observation) recordCut() (recordCut, error) {
	switch {
	case !observation.record.present && !observation.temporary.present:
		return recordAbsent, nil
	case observation.record.present && observation.record.size == 0 && !observation.temporary.present:
		return recordOld, nil
	case observation.record.present && observation.record.size == 0 &&
		observation.temporary.present && observation.temporary.size == 0:
		return recordTemporaryCreated, nil
	case observation.record.present && observation.record.size == 0 &&
		observation.temporary.present && observation.temporary.size == 1:
		return recordTemporaryReady, nil
	case observation.record.present && observation.record.size == 1 && !observation.temporary.present:
		return recordInstalled, nil
	default:
		return 0, errors.New("probe records do not form a reachable replacement cut")
	}
}

func (observation Observation) directoryCut() directoryCut {
	switch {
	case observation.candidate && observation.installed:
		return directoriesInstalledAndCandidate
	case observation.candidate:
		return directoryCandidate
	case observation.installed:
		return directoryInstalled
	default:
		return directoriesAbsent
	}
}
