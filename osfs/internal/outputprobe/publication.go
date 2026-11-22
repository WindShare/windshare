package outputprobe

import (
	"errors"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

type filePublisher interface {
	PublishFileNoReplace(outputcap.FileIdentity, string) (outputcap.PublishNoReplaceOutcome, error)
}

// FilePublication exercises the atomic no-replace file publication primitive.
func (workspace *Workspace) FilePublication() error {
	if err := workspace.requireOpen(); err != nil {
		return err
	}
	publisher, ok := workspace.directory.(filePublisher)
	if !ok {
		return outputcap.ErrOrdinaryOutputUnsupported
	}
	source, err := workspace.createFile(StageName, 0)
	if err != nil {
		return err
	}
	workspace.files[PublicationName] = append(workspace.files[PublicationName], source)
	outcome, err := publisher.PublishFileNoReplace(source.file, PublicationName)
	if err != nil {
		return err
	}
	if outcome != outputcap.PublishNoReplaceCommitted {
		return unsafe("probe file publication", errors.New("absent target was not published"))
	}
	if err := workspace.verifyPublishedFile(source); err != nil {
		return err
	}
	collision, err := workspace.createFile(AnchorName, 0)
	if err != nil {
		return err
	}
	// Retain both possible images before testing a provider. A broken no-replace
	// primitive may overwrite our own target even when it reports an error.
	workspace.files[PublicationName] = append(workspace.files[PublicationName], collision)
	outcome, err = publisher.PublishFileNoReplace(collision.file, PublicationName)
	if err != nil {
		return err
	}
	if outcome != outputcap.PublishNoReplaceCollision {
		return unsupported("probe file publication", errors.New("existing target was not protected"))
	}
	if err := workspace.verifyPublishedFile(source); err != nil {
		return err
	}
	return workspace.VerifyFile(AnchorName, false)
}

// OperationRecovery exercises the atomic no-replace directory installation primitive.
func (workspace *Workspace) OperationRecovery() error {
	if err := workspace.requireOpen(); err != nil {
		return err
	}
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

func (workspace *Workspace) createFile(name string, size int64) (*retainedFile, error) {
	// A failed create can leave an entry even without returning a handle.
	// Keep its place in cleanup ordering so an unowned entry fails closed.
	if _, exists := workspace.files[name]; !exists {
		workspace.files[name] = nil
	}
	file, err := workspace.directory.CreateFile(name, true, size)
	if file == nil {
		if err == nil {
			err = unsafe("create probe file "+name, errors.New("creation returned no file"))
		}
		return nil, err
	}
	witness := workspace.retainFile(name, file)
	if err != nil {
		return witness, err
	}
	return witness, errors.Join(file.Sync(), workspace.directory.Sync())
}

func (workspace *Workspace) VerifyFile(name string, private bool) error {
	if err := workspace.requireOpen(); err != nil {
		return err
	}
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

func (workspace *Workspace) verifyPublishedFile(expected *retainedFile) error {
	current, err := workspace.directory.OpenObservedFile(PublicationName, false)
	if err != nil {
		return err
	}
	same, compareErr := current.SameFile(expected.file)
	closeErr := current.Close()
	if compareErr != nil || closeErr != nil {
		return errors.Join(compareErr, closeErr)
	}
	if !same {
		return unsafe("probe file publication", errors.New("published target changed identity"))
	}
	return nil
}
