package outputprobe

import (
	"errors"
	"fmt"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

// Recover validates every bounded leftover before the first deletion. One
// foreign or malformed namespace cannot cause earlier valid entries to vanish.
func Recover(root outputcap.Directory, names []string, profile Profile) (resultErr error) {
	leftovers := make([]*Workspace, 0, len(names))
	defer func() {
		for _, leftover := range leftovers {
			resultErr = errors.Join(resultErr, leftover.Close())
		}
	}()
	if len(names) > MaximumLeftovers {
		return unsafe("recover probe", errors.New("leftover count exceeds its bound"))
	}
	for _, name := range names {
		if !CanonicalName(name) {
			return unsafe("recover probe", fmt.Errorf("malformed reserved probe name %q", name))
		}
		leftover, err := Inspect(root, name, profile)
		if err != nil {
			return unsafe("inspect probe leftover "+name, err)
		}
		leftovers = append(leftovers, leftover)
	}
	for _, leftover := range leftovers {
		if err := leftover.Cleanup(); err != nil {
			return unsafe("remove probe leftover "+leftover.name, err)
		}
	}
	return nil
}

func Inspect(root outputcap.Directory, name string, profile Profile) (_ *Workspace, resultErr error) {
	directory, err := root.OpenDirectory(name, true)
	if err != nil {
		return nil, err
	}
	workspace := NewWorkspace(root, name, directory)
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, workspace.Close())
		}
	}()
	maximumEntries := len(fileRemovalOrder) + len(directoryRemovalOrder)
	if profile != LinuxExt4 {
		maximumEntries--
	}
	names, err := directory.Names(maximumEntries)
	if err != nil {
		return nil, err
	}
	inspection := leftoverInspection{workspace: workspace, profile: profile}
	for _, entry := range names {
		if err := inspection.inspectEntry(entry); err != nil {
			return nil, err
		}
	}
	if err := Validate(profile, inspection.observation); err != nil {
		return nil, err
	}
	return workspace, nil
}

type leftoverInspection struct {
	workspace   *Workspace
	profile     Profile
	observation Observation
	dataWitness outputcap.FileIdentity
}

func (inspection *leftoverInspection) inspectEntry(entry string) error {
	switch entry {
	case StageName, AnchorName, PublicationName, RecordName, TemporaryRecordName, PublicStageName:
		return inspection.inspectFile(entry)
	case CandidateName, InstalledName:
		return inspection.inspectDirectory(entry)
	default:
		return fmt.Errorf("unexpected probe entry %q", entry)
	}
}

func (inspection *leftoverInspection) inspectFile(entry string) error {
	public := entry == PublicStageName
	if public && inspection.profile != LinuxExt4 {
		return fmt.Errorf("unexpected probe entry %q", entry)
	}
	file, err := inspection.workspace.directory.OpenObservedFile(entry, !public)
	if err != nil {
		return err
	}
	inspection.workspace.retainFile(entry, file)
	size, err := file.Size()
	if err != nil {
		return err
	}
	if !public {
		if err := inspection.observation.ObserveFile(entry, size); err != nil {
			return err
		}
	}
	if entry == StageName || entry == AnchorName || entry == PublicationName {
		if inspection.dataWitness == nil {
			inspection.dataWitness = file
		} else if same, err := inspection.dataWitness.SameFile(file); err != nil || !same {
			return unsafe("inspect probe links", errors.Join(errors.New("data links are not one object"), err))
		}
	}
	return nil
}

func (inspection *leftoverInspection) inspectDirectory(entry string) error {
	child, err := inspection.workspace.directory.OpenDirectory(entry, true)
	if err != nil {
		return err
	}
	inspection.workspace.retainDirectory(entry, child)
	names, err := child.Names(0)
	if err != nil || len(names) != 0 {
		return errors.Join(fmt.Errorf("probe directory %q is not empty", entry), err)
	}
	return inspection.observation.ObserveDirectory(entry)
}
