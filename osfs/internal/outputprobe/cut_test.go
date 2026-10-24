package outputprobe

import (
	"fmt"
	"testing"
)

func TestOutputV3ProbeCutAcceptsEveryReachablePhase(t *testing.T) {
	t.Parallel()
	profiles := []struct {
		name    string
		profile Profile
		count   int
	}{
		{name: "linux-ext4", profile: LinuxExt4, count: 37},
		{name: "windows-ntfs", profile: WindowsNTFS, count: 38},
	}
	for _, profile := range profiles {
		t.Run(profile.name, func(t *testing.T) {
			t.Parallel()
			allData := []Observation{
				probeCutData(profile.profile, dataAbsent),
				probeCutData(profile.profile, dataStage),
				probeCutData(profile.profile, dataStageAnchor),
				probeCutData(profile.profile, dataStageAnchorPublication),
				probeCutData(profile.profile, dataAnchorPublication),
				probeCutData(profile.profile, dataAnchor),
			}
			observations := append([]Observation(nil), allData...)
			if profile.profile == WindowsNTFS {
				zeroStage := probeCutData(profile.profile, dataStage)
				zeroStage.stage.size = 0
				observations = append(observations, zeroStage)
			}

			lateData := []Observation{
				probeCutData(profile.profile, dataAbsent),
				probeCutData(profile.profile, dataStageAnchorPublication),
				probeCutData(profile.profile, dataAnchorPublication),
				probeCutData(profile.profile, dataAnchor),
			}
			records := []Observation{
				probeCutRecord(recordOld),
				probeCutRecord(recordTemporaryCreated),
				probeCutRecord(recordTemporaryReady),
				probeCutRecord(recordInstalled),
			}
			for _, record := range records {
				for _, data := range lateData {
					observations = append(observations, mergeProbeCutObservations(data, record))
				}
			}

			directories := []Observation{
				probeCutDirectories(directoryCandidate),
				probeCutDirectories(directoryInstalled),
				probeCutDirectories(directoriesInstalledAndCandidate),
			}
			installedRecord := probeCutRecord(recordInstalled)
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
				if err := Validate(profile.profile, observation); err != nil {
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
		profile Profile
	}{
		{name: "linux-ext4", profile: LinuxExt4},
		{name: "windows-ntfs", profile: WindowsNTFS},
	}
	for _, profile := range profiles {
		t.Run(profile.name, func(t *testing.T) {
			t.Parallel()
			bytes := uint64(0)
			if profile.profile == WindowsNTFS {
				bytes = 1
			}
			forbidden := map[string]Observation{
				"publication without anchor": {
					publication: observedFile{present: true, size: bytes},
				},
				"stage and publication without anchor": {
					stage:       observedFile{present: true, size: bytes},
					publication: observedFile{present: true, size: bytes},
				},
				"record after stage only": mergeProbeCutObservations(
					probeCutData(profile.profile, dataStage),
					probeCutRecord(recordOld),
				),
				"record after stage and anchor": mergeProbeCutObservations(
					probeCutData(profile.profile, dataStageAnchor),
					probeCutRecord(recordInstalled),
				),
				"candidate before installed record": mergeProbeCutObservations(
					probeCutRecord(recordOld),
					probeCutDirectories(directoryCandidate),
				),
				"installed directory during temporary record": mergeProbeCutObservations(
					probeCutRecord(recordTemporaryReady),
					probeCutDirectories(directoryInstalled),
				),
				"directory after record cleanup with data link": mergeProbeCutObservations(
					probeCutData(profile.profile, dataAnchor),
					probeCutDirectories(directoryCandidate),
				),
				"temporary without old record": {
					temporary: observedFile{present: true, size: 0},
				},
				"temporary beside installed record": {
					record:    observedFile{present: true, size: 1},
					temporary: observedFile{present: true, size: 1},
				},
				"oversized record": {
					record: observedFile{present: true, size: 2},
				},
				"oversized temporary": {
					record:    observedFile{present: true, size: 0},
					temporary: observedFile{present: true, size: 2},
				},
			}
			for name, observation := range forbidden {
				if err := Validate(profile.profile, observation); err == nil {
					t.Errorf("%s was accepted", name)
				}
			}
		})
	}

	windowsZeroLinkedStage := probeCutData(WindowsNTFS, dataStageAnchor)
	windowsZeroLinkedStage.stage.size = 0
	if err := Validate(WindowsNTFS, windowsZeroLinkedStage); err == nil {
		t.Error("Windows zero-length stage was accepted after hard-link creation")
	}
	windowsZeroAnchor := probeCutData(WindowsNTFS, dataAnchor)
	windowsZeroAnchor.anchor.size = 0
	if err := Validate(WindowsNTFS, windowsZeroAnchor); err == nil {
		t.Error("Windows zero-length anchor was accepted")
	}
	linuxNonzeroStage := probeCutData(LinuxExt4, dataStage)
	linuxNonzeroStage.stage.size = 1
	if err := Validate(LinuxExt4, linuxNonzeroStage); err == nil {
		t.Error("Linux nonzero probe stage was accepted")
	}
	if err := Validate(0, Observation{}); err == nil {
		t.Error("unknown probe data profile was accepted")
	}
}

func probeCutData(
	profile Profile,
	cut dataCut,
) Observation {
	bytes := uint64(0)
	if profile == WindowsNTFS {
		bytes = 1
	}
	file := observedFile{present: true, size: bytes}
	var observation Observation
	switch cut {
	case dataAbsent:
	case dataStage:
		observation.stage = file
	case dataStageAnchor:
		observation.stage, observation.anchor = file, file
	case dataStageAnchorPublication:
		observation.stage, observation.anchor, observation.publication = file, file, file
	case dataAnchorPublication:
		observation.anchor, observation.publication = file, file
	case dataAnchor:
		observation.anchor = file
	default:
		panic(fmt.Sprintf("unknown test data cut %d", cut))
	}
	return observation
}

func probeCutRecord(cut recordCut) Observation {
	var observation Observation
	switch cut {
	case recordAbsent:
	case recordOld:
		observation.record = observedFile{present: true, size: 0}
	case recordTemporaryCreated:
		observation.record = observedFile{present: true, size: 0}
		observation.temporary = observedFile{present: true, size: 0}
	case recordTemporaryReady:
		observation.record = observedFile{present: true, size: 0}
		observation.temporary = observedFile{present: true, size: 1}
	case recordInstalled:
		observation.record = observedFile{present: true, size: 1}
	default:
		panic(fmt.Sprintf("unknown test record cut %d", cut))
	}
	return observation
}

func probeCutDirectories(cut directoryCut) Observation {
	var observation Observation
	switch cut {
	case directoriesAbsent:
	case directoryCandidate:
		observation.candidate = true
	case directoryInstalled:
		observation.installed = true
	case directoriesInstalledAndCandidate:
		observation.candidate, observation.installed = true, true
	default:
		panic(fmt.Sprintf("unknown test directory cut %d", cut))
	}
	return observation
}

func mergeProbeCutObservations(
	observations ...Observation,
) Observation {
	var merged Observation
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
