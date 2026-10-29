//go:build darwin

package outputdarwin

import (
	"errors"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

// Entry primitives stop at the successful native mutation. Keeping observation
// and durability separate preserves the evidence needed to release reservations.
type filePublisher interface {
	linkFileEntryNoReplace(outputcap.FileIdentity, string) error
	openLinkedFile(outputcap.FileIdentity, string) (outputcap.ObservedFile, error)
	Sync() error
}

type publicDirectoryReserver interface {
	createDirectoryEntry(string, uint32) error
	OpenDirectory(string, bool) (outputcap.Directory, error)
	Sync() error
}

func publishFileNoReplace(dir filePublisher, source outputcap.FileIdentity, name string) (outputcap.PublishNoReplaceOutcome, error) {
	err := dir.linkFileEntryNoReplace(source, name)
	if errors.Is(err, outputcap.ErrNamespaceCollision) {
		return outputcap.PublishNoReplaceCollision, nil
	}
	if err != nil {
		return 0, err
	}
	published, err := dir.openLinkedFile(source, name)
	if err != nil {
		return outputcap.PublishNoReplaceIndeterminate, err
	}
	if err := errors.Join(published.Close(), dir.Sync()); err != nil {
		return outputcap.PublishNoReplaceIndeterminate, err
	}
	return outputcap.PublishNoReplaceCommitted, nil
}

func reservePublicDirectoryNoReplace(dir publicDirectoryReserver, name string) (outputcap.Directory, outputcap.PublishNoReplaceOutcome, error) {
	err := dir.createDirectoryEntry(name, publicDirectoryMode)
	if errors.Is(err, outputcap.ErrNamespaceCollision) {
		return nil, outputcap.PublishNoReplaceCollision, nil
	}
	if err != nil {
		return nil, 0, err
	}
	created, err := dir.OpenDirectory(name, false)
	if err != nil {
		return nil, outputcap.PublishNoReplaceIndeterminate, err
	}
	if err := errors.Join(created.Sync(), dir.Sync()); err != nil {
		return nil, outputcap.PublishNoReplaceIndeterminate, errors.Join(err, created.Close())
	}
	return created, outputcap.PublishNoReplaceCommitted, nil
}
