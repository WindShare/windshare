package osfs

import (
	"fmt"
	"testing"
)

func TestOutputV3ProbeCutAcceptsEveryReachablePhase(t *testing.T) {
	t.Parallel()
	profiles := []struct {
		name    string
		profile outputV3ProbeDataProfile
		count   int
	}{
		{name: "linux-ext4", profile: outputV3ProbeDataLinuxExt4, count: 37},
		{name: "windows-ntfs", profile: outputV3ProbeDataWindowsNTFS, count: 38},
	}
	for _, profile := range profiles {
		profile := profile
		t.Run(profile.name, func(t *testing.T) {
			t.Parallel()
			allData := []outputV3ProbeCutObservation{
				probeCutData(profile.profile, outputV3ProbeDataAbsent),
				probeCutData(profile.profile, outputV3ProbeDataStage),
				probeCutData(profile.profile, outputV3ProbeDataStageAnchor),
				probeCutData(profile.profile, outputV3ProbeDataStageAnchorPublication),
				probeCutData(profile.profile, outputV3ProbeDataAnchorPublication),
				probeCutData(profile.profile, outputV3ProbeDataAnchor),
			}
			observations := append([]outputV3ProbeCutObservation(nil), allData...)
			if profile.profile == outputV3ProbeDataWindowsNTFS {
				zeroStage := probeCutData(profile.profile, outputV3ProbeDataStage)
				zeroStage.stage.size = 0
				observations = append(observations, zeroStage)
			}

			lateData := []outputV3ProbeCutObservation{
				probeCutData(profile.profile, outputV3ProbeDataAbsent),
				probeCutData(profile.profile, outputV3ProbeDataStageAnchorPublication),
				probeCutData(profile.profile, outputV3ProbeDataAnchorPublication),
				probeCutData(profile.profile, outputV3ProbeDataAnchor),
			}
			records := []outputV3ProbeCutObservation{
				probeCutRecord(outputV3ProbeRecordOld),
				probeCutRecord(outputV3ProbeRecordTemporaryCreated),
				probeCutRecord(outputV3ProbeRecordTemporaryReady),
				probeCutRecord(outputV3ProbeRecordInstalled),
			}
			for _, record := range records {
				for _, data := range lateData {
					observations = append(observations, mergeProbeCutObservations(data, record))
				}
			}

			directories := []outputV3ProbeCutObservation{
				probeCutDirectories(outputV3ProbeDirectoryCandidate),
				probeCutDirectories(outputV3ProbeDirectoryInstalled),
				probeCutDirectories(outputV3ProbeDirectoriesInstalledAndCandidate),
			}
			installedRecord := probeCutRecord(outputV3ProbeRecordInstalled)
			for _, directory := range directories {
				for _, data := range lateData {
					observations = append(observations,
						mergeProbeCutObservations(data, installedRecord, directory))
				}
				observations = append(observations, directory)
			}

			if len(observations) != profile.count {
				t.Fatalf("test model produced %d cuts, want %d", len(observations), profile.count)
			}
			for index, observation := range observations {
				if err := validateOutputV3ProbeCut(profile.profile, observation); err != nil {
					t.Errorf("reachable cut %d rejected: %v", index, err)
				}
			}
		})
	}
}

func TestOutputV3ProbeCutRejectsImpossibleOrAmbiguousPhases(t *testing.T) {
	t.Parallel()
	profiles := []struct {
		name    string
		profile outputV3ProbeDataProfile
	}{
		{name: "linux-ext4", profile: outputV3ProbeDataLinuxExt4},
		{name: "windows-ntfs", profile: outputV3ProbeDataWindowsNTFS},
	}
	for _, profile := range profiles {
		profile := profile
		t.Run(profile.name, func(t *testing.T) {
			t.Parallel()
			bytes := uint64(0)
			if profile.profile == outputV3ProbeDataWindowsNTFS {
				bytes = 1
			}
			forbidden := map[string]outputV3ProbeCutObservation{
				"publication without anchor": {
					publication: outputV3ProbeObservedFile{present: true, size: bytes},
				},
				"stage and publication without anchor": {
					stage:       outputV3ProbeObservedFile{present: true, size: bytes},
					publication: outputV3ProbeObservedFile{present: true, size: bytes},
				},
				"record after stage only": mergeProbeCutObservations(
					probeCutData(profile.profile, outputV3ProbeDataStage),
					probeCutRecord(outputV3ProbeRecordOld),
				),
				"record after stage and anchor": mergeProbeCutObservations(
					probeCutData(profile.profile, outputV3ProbeDataStageAnchor),
					probeCutRecord(outputV3ProbeRecordInstalled),
				),
				"candidate before installed record": mergeProbeCutObservations(
					probeCutRecord(outputV3ProbeRecordOld),
					probeCutDirectories(outputV3ProbeDirectoryCandidate),
				),
				"installed directory during temporary record": mergeProbeCutObservations(
					probeCutRecord(outputV3ProbeRecordTemporaryReady),
					probeCutDirectories(outputV3ProbeDirectoryInstalled),
				),
				"directory after record cleanup with data link": mergeProbeCutObservations(
					probeCutData(profile.profile, outputV3ProbeDataAnchor),
					probeCutDirectories(outputV3ProbeDirectoryCandidate),
				),
				"temporary without old record": {
					temporary: outputV3ProbeObservedFile{present: true, size: 0},
				},
				"temporary beside installed record": {
					record:    outputV3ProbeObservedFile{present: true, size: 1},
					temporary: outputV3ProbeObservedFile{present: true, size: 1},
				},
				"oversized record": {
					record: outputV3ProbeObservedFile{present: true, size: 2},
				},
				"oversized temporary": {
					record:    outputV3ProbeObservedFile{present: true, size: 0},
					temporary: outputV3ProbeObservedFile{present: true, size: 2},
				},
			}
			for name, observation := range forbidden {
				if err := validateOutputV3ProbeCut(profile.profile, observation); err == nil {
					t.Errorf("%s was accepted", name)
				}
			}
		})
	}

	windowsZeroLinkedStage := probeCutData(outputV3ProbeDataWindowsNTFS, outputV3ProbeDataStageAnchor)
	windowsZeroLinkedStage.stage.size = 0
	if err := validateOutputV3ProbeCut(outputV3ProbeDataWindowsNTFS, windowsZeroLinkedStage); err == nil {
		t.Error("Windows zero-length stage was accepted after hard-link creation")
	}
	windowsZeroAnchor := probeCutData(outputV3ProbeDataWindowsNTFS, outputV3ProbeDataAnchor)
	windowsZeroAnchor.anchor.size = 0
	if err := validateOutputV3ProbeCut(outputV3ProbeDataWindowsNTFS, windowsZeroAnchor); err == nil {
		t.Error("Windows zero-length anchor was accepted")
	}
	linuxNonzeroStage := probeCutData(outputV3ProbeDataLinuxExt4, outputV3ProbeDataStage)
	linuxNonzeroStage.stage.size = 1
	if err := validateOutputV3ProbeCut(outputV3ProbeDataLinuxExt4, linuxNonzeroStage); err == nil {
		t.Error("Linux nonzero probe stage was accepted")
	}
	if err := validateOutputV3ProbeCut(0, outputV3ProbeCutObservation{}); err == nil {
		t.Error("unknown probe data profile was accepted")
	}
}

func probeCutData(
	profile outputV3ProbeDataProfile,
	cut outputV3ProbeDataCut,
) outputV3ProbeCutObservation {
	bytes := uint64(0)
	if profile == outputV3ProbeDataWindowsNTFS {
		bytes = 1
	}
	file := outputV3ProbeObservedFile{present: true, size: bytes}
	var observation outputV3ProbeCutObservation
	switch cut {
	case outputV3ProbeDataAbsent:
	case outputV3ProbeDataStage:
		observation.stage = file
	case outputV3ProbeDataStageAnchor:
		observation.stage, observation.anchor = file, file
	case outputV3ProbeDataStageAnchorPublication:
		observation.stage, observation.anchor, observation.publication = file, file, file
	case outputV3ProbeDataAnchorPublication:
		observation.anchor, observation.publication = file, file
	case outputV3ProbeDataAnchor:
		observation.anchor = file
	default:
		panic(fmt.Sprintf("unknown test data cut %d", cut))
	}
	return observation
}

func probeCutRecord(cut outputV3ProbeRecordCut) outputV3ProbeCutObservation {
	var observation outputV3ProbeCutObservation
	switch cut {
	case outputV3ProbeRecordAbsent:
	case outputV3ProbeRecordOld:
		observation.record = outputV3ProbeObservedFile{present: true, size: 0}
	case outputV3ProbeRecordTemporaryCreated:
		observation.record = outputV3ProbeObservedFile{present: true, size: 0}
		observation.temporary = outputV3ProbeObservedFile{present: true, size: 0}
	case outputV3ProbeRecordTemporaryReady:
		observation.record = outputV3ProbeObservedFile{present: true, size: 0}
		observation.temporary = outputV3ProbeObservedFile{present: true, size: 1}
	case outputV3ProbeRecordInstalled:
		observation.record = outputV3ProbeObservedFile{present: true, size: 1}
	default:
		panic(fmt.Sprintf("unknown test record cut %d", cut))
	}
	return observation
}

func probeCutDirectories(cut outputV3ProbeDirectoryCut) outputV3ProbeCutObservation {
	var observation outputV3ProbeCutObservation
	switch cut {
	case outputV3ProbeDirectoriesAbsent:
	case outputV3ProbeDirectoryCandidate:
		observation.candidate = true
	case outputV3ProbeDirectoryInstalled:
		observation.installed = true
	case outputV3ProbeDirectoriesInstalledAndCandidate:
		observation.candidate, observation.installed = true, true
	default:
		panic(fmt.Sprintf("unknown test directory cut %d", cut))
	}
	return observation
}

func mergeProbeCutObservations(
	observations ...outputV3ProbeCutObservation,
) outputV3ProbeCutObservation {
	var merged outputV3ProbeCutObservation
	for _, observation := range observations {
		if observation.stage.present {
			merged.stage = observation.stage
		}
		if observation.anchor.present {
			merged.anchor = observation.anchor
		}
		if observation.publication.present {
			merged.publication = observation.publication
		}
		if observation.record.present {
			merged.record = observation.record
		}
		if observation.temporary.present {
			merged.temporary = observation.temporary
		}
		merged.candidate = merged.candidate || observation.candidate
		merged.installed = merged.installed || observation.installed
	}
	return merged
}
