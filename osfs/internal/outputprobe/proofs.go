package outputprobe

import (
	"errors"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

const (
	replacementRecordSize = 1
	sizedDataLinkSize     = 1
)

func (workspace *Workspace) SafePublish(profile Profile) error {
	var size int64
	switch profile {
	case LinuxExt4:
	case WindowsNTFS:
		size = sizedDataLinkSize
	default:
		return unsupported("probe safe publication", errors.New("unknown probe profile"))
	}
	stage, err := workspace.createFile(StageName, size)
	if err != nil {
		return err
	}
	anchor, err := workspace.link(stage, AnchorName)
	if err != nil {
		return err
	}
	if _, err := workspace.link(anchor, PublicationName); err != nil {
		return err
	}
	unexpected, err := workspace.directory.LinkFileNoReplace(anchor.file, PublicationName)
	if unexpected != nil {
		workspace.retainFile(PublicationName, unexpected)
	}
	if !errors.Is(err, outputcap.ErrNamespaceCollision) {
		return unsupported("probe safe publication", errors.Join(errors.New("publication is not atomic no-replace"), err))
	}
	return nil
}

func (workspace *Workspace) createFile(name string, size int64) (*retainedFile, error) {
	file, err := workspace.directory.CreateFile(name, true, size)
	if file == nil {
		return nil, err
	}
	witness := workspace.retainFile(name, file)
	if err != nil {
		return witness, err
	}
	return witness, errors.Join(file.Sync(), workspace.directory.Sync())
}

func (workspace *Workspace) link(source *retainedFile, name string) (*retainedFile, error) {
	workspace.files[name] = append(workspace.files[name], source)
	file, err := workspace.directory.LinkFileNoReplace(source.file, name)
	if file == nil {
		return nil, err
	}
	witness := workspace.retainFile(name, file)
	if err != nil {
		return witness, err
	}
	same, err := source.file.SameFile(file)
	if err != nil || !same {
		return witness, unsafe("probe hard link", errors.Join(errors.New("linked name is not the source object"), err))
	}
	return witness, nil
}

func (workspace *Workspace) RangeRecovery() error {
	if _, err := workspace.createFile(RecordName, 0); err != nil {
		return err
	}
	newRecord, err := workspace.createFile(TemporaryRecordName, replacementRecordSize)
	if err != nil {
		return err
	}
	// Keep both generations live until cleanup: replacement can fail before or
	// after its mutation, and neither outcome grants authority over a new name.
	// Native replacement permits these handles (including delete sharing on NTFS).
	workspace.files[RecordName] = append(workspace.files[RecordName], newRecord)
	if err := workspace.directory.ReplacePrivateFile(newRecord.file, RecordName); err != nil {
		return err
	}
	delete(workspace.files, TemporaryRecordName)
	if err := workspace.directory.Sync(); err != nil {
		return err
	}
	current, err := workspace.directory.OpenObservedFile(RecordName, true)
	if err != nil {
		return err
	}
	same, compareErr := newRecord.file.SameFile(current)
	closeErr := current.Close()
	if compareErr != nil || closeErr != nil || !same {
		return unsafe("probe record replacement", errors.Join(errors.New("installed record is not the expected generation"), compareErr, closeErr))
	}
	return nil
}

func (workspace *Workspace) OperationRecovery() error {
	candidate, err := workspace.directory.CreateDirectory(CandidateName, true)
	if err != nil {
		return err
	}
	witness := workspace.retainDirectory(CandidateName, candidate)
	if err := errors.Join(candidate.Sync(), workspace.directory.Sync()); err != nil {
		return err
	}
	workspace.directories[InstalledName] = append(workspace.directories[InstalledName], witness)
	installed, err := workspace.directory.InstallDirectoryNoReplace(candidate, InstalledName)
	if installed != nil {
		workspace.retainDirectory(InstalledName, installed)
	}
	if err != nil {
		return err
	}
	delete(workspace.directories, CandidateName)
	if err := workspace.directory.Sync(); err != nil {
		return err
	}
	collision, err := workspace.directory.CreateDirectory(CandidateName, true)
	if err != nil {
		return err
	}
	workspace.retainDirectory(CandidateName, collision)
	unexpected, err := workspace.directory.InstallDirectoryNoReplace(collision, InstalledName)
	if unexpected != nil {
		workspace.retainDirectory(InstalledName, unexpected)
	}
	if !errors.Is(err, outputcap.ErrNamespaceCollision) {
		return unsupported("probe directory installation", errors.Join(errors.New("directory installation is not atomic no-replace"), err))
	}
	return nil
}

// VerifyFiles proves that reopening cleanup names preserves the retained object
// identities. It does not infer restart guarantees from successful live I/O.
func (workspace *Workspace) VerifyFiles() error {
	for _, name := range [...]string{StageName, AnchorName, PublicationName, RecordName} {
		if err := workspace.VerifyFile(name, true); err != nil {
			return err
		}
	}
	return nil
}

func (workspace *Workspace) VerifyFile(name string, private bool) error {
	witnesses := workspace.files[name]
	if len(witnesses) == 0 {
		return unsafe("probe cleanup identity", errors.New("required probe witness is absent"))
	}
	current, err := workspace.directory.OpenObservedFile(name, private)
	if err != nil {
		return err
	}
	expected := witnesses[len(witnesses)-1]
	same, compareErr := current.SameFile(expected.file)
	closeErr := current.Close()
	if compareErr != nil || closeErr != nil || !same {
		return unsafe("probe cleanup identity", errors.Join(errors.New("cleanup name does not identify its retained object"), compareErr, closeErr))
	}
	return nil
}
