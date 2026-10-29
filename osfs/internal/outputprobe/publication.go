package outputprobe

import (
	"errors"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

type filePublisher interface {
	PublishFileNoReplace(outputcap.FileIdentity, string) (outputcap.PublishNoReplaceOutcome, error)
}

// FilePublication exercises the publication primitive used by the receiver.
// Hard links and restart identity are independent and cannot gate this proof.
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
