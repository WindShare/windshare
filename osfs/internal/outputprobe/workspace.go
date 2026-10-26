package outputprobe

import (
	"errors"
	"io/fs"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

type retainedFile struct {
	file   outputcap.FileIdentity
	closed bool
}

type retainedDirectory struct {
	directory outputcap.Directory
}

// Workspace owns the probe's children and directory, borrowing only its root.
// Possible target names are recorded before mutation: an error after a native
// rename/link must not erase the witness needed for exact-object cleanup.
type Workspace struct {
	root             outputcap.Directory
	name             string
	directory        outputcap.Directory
	files            map[string][]*retainedFile
	directories      map[string][]*retainedDirectory
	ownedFiles       []*retainedFile
	ownedDirectories []*retainedDirectory
}

func NewWorkspace(root outputcap.Directory, name string, directory outputcap.Directory) *Workspace {
	return &Workspace{
		root: root, name: name, directory: directory,
		files:       make(map[string][]*retainedFile),
		directories: make(map[string][]*retainedDirectory),
	}
}

func (workspace *Workspace) Directory() outputcap.Directory { return workspace.directory }

// KeepFile transfers the original creation witness even when installation
// returned an error. Reopening a name alone cannot prove cleanup ownership.
func (workspace *Workspace) KeepFile(name string, file outputcap.FileIdentity) {
	if file != nil {
		workspace.retainFile(name, file)
	}
}

func (workspace *Workspace) retainFile(name string, file outputcap.FileIdentity) *retainedFile {
	witness := &retainedFile{file: file}
	workspace.ownedFiles = append(workspace.ownedFiles, witness)
	workspace.files[name] = append(workspace.files[name], witness)
	return witness
}

func (workspace *Workspace) retainDirectory(name string, directory outputcap.Directory) *retainedDirectory {
	witness := &retainedDirectory{directory: directory}
	workspace.ownedDirectories = append(workspace.ownedDirectories, witness)
	workspace.directories[name] = append(workspace.directories[name], witness)
	return witness
}

func (workspace *Workspace) CleanupEntries() error {
	for _, name := range fileRemovalOrder {
		witnesses, exists := workspace.files[name]
		if !exists {
			continue
		}
		if err := workspace.removeFile(name, witnesses); err != nil {
			return err
		}
		delete(workspace.files, name)
	}
	for _, name := range directoryRemovalOrder {
		witnesses, exists := workspace.directories[name]
		if !exists {
			continue
		}
		if err := workspace.removeDirectory(name, witnesses); err != nil {
			return err
		}
		delete(workspace.directories, name)
	}
	return nil
}

func (workspace *Workspace) removeFile(name string, witnesses []*retainedFile) (resultErr error) {
	current, err := workspace.directory.OpenObservedFile(name, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, current.Close()) }()
	for _, witness := range witnesses {
		if witness.closed {
			continue
		}
		same, err := current.SameFile(witness.file)
		if err != nil {
			return err
		}
		if same {
			if err := workspace.directory.RemoveFile(name, witness.file); err != nil {
				return err
			}
			return workspace.directory.Sync()
		}
	}
	return unsafe("clean probe file", errors.New("entry does not match a retained creation witness"))
}

func (workspace *Workspace) removeDirectory(name string, witnesses []*retainedDirectory) (resultErr error) {
	current, err := workspace.directory.OpenDirectory(name, true)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, current.Close()) }()
	for _, witness := range witnesses {
		same, err := current.SameDirectory(witness.directory)
		if err != nil {
			return err
		}
		if same {
			if err := workspace.directory.RemoveDirectory(name, witness.directory); err != nil {
				return err
			}
			return workspace.directory.Sync()
		}
	}
	return unsafe("clean probe directory", errors.New("entry does not match a retained creation witness"))
}

// Clear ends one independent capability proof before another reuses its names.
// Cleanup always stops at the first unverified mutation; later artifacts retain
// the reachable recovery cut instead of being partially dismantled.
func (workspace *Workspace) Clear() error {
	if err := workspace.CleanupEntries(); err != nil {
		return err
	}
	return workspace.closeChildren()
}

func (workspace *Workspace) Cleanup() error {
	if workspace == nil {
		return nil
	}
	if workspace.directory == nil {
		return workspace.closeChildren()
	}
	if err := workspace.Clear(); err != nil {
		return errors.Join(err, workspace.Close())
	}
	if workspace.root != nil {
		if err := workspace.root.RemoveDirectory(workspace.name, workspace.directory); err != nil {
			return errors.Join(err, workspace.Close())
		}
		if err := workspace.root.Sync(); err != nil {
			return errors.Join(err, workspace.Close())
		}
	}
	return workspace.Close()
}

func (workspace *Workspace) closeChildren() error {
	var result error
	for _, witness := range workspace.ownedFiles {
		if !witness.closed {
			result = errors.Join(result, witness.file.Close())
			witness.closed = true
		}
	}
	for _, witness := range workspace.ownedDirectories {
		result = errors.Join(result, witness.directory.Close())
	}
	workspace.ownedFiles = nil
	workspace.ownedDirectories = nil
	return result
}

// Close releases inspection/creation handles without authorizing any deletion.
func (workspace *Workspace) Close() error {
	if workspace == nil {
		return nil
	}
	err := workspace.closeChildren()
	if workspace.directory != nil {
		err = errors.Join(err, workspace.directory.Close())
		workspace.directory = nil
	}
	return err
}
