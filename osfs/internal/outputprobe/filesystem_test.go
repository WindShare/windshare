package outputprobe

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

var errInjected = errors.New("injected native operation failure")

type probeTestFileObject struct {
	size uint64
}

type probeTestDirectoryObject struct {
	files       map[string]*probeTestFileObject
	directories map[string]*probeTestDirectoryObject
}

type probeTestFilesystem struct {
	events      []string
	fault       string
	openHandles int
}

func (system *probeTestFilesystem) event(event string) error {
	system.events = append(system.events, event)
	if system.fault == event {
		system.fault = ""
		return errInjected
	}
	return nil
}

func newProbeTestDirectoryObject() *probeTestDirectoryObject {
	return &probeTestDirectoryObject{
		files:       make(map[string]*probeTestFileObject),
		directories: make(map[string]*probeTestDirectoryObject),
	}
}

type probeTestDirectory struct {
	outputcap.Directory
	system *probeTestFilesystem
	object *probeTestDirectoryObject
	closed bool
}

func newProbeTestRoot() *probeTestDirectory {
	return newProbeTestDirectory(&probeTestFilesystem{}, newProbeTestDirectoryObject())
}

func newProbeTestDirectory(system *probeTestFilesystem, object *probeTestDirectoryObject) *probeTestDirectory {
	system.openHandles++
	return &probeTestDirectory{system: system, object: object}
}

func (directory *probeTestDirectory) Close() error {
	if !directory.closed {
		directory.closed = true
		directory.system.openHandles--
	}
	return nil
}

func (directory *probeTestDirectory) Sync() error { return directory.system.event("sync-directory") }

func (directory *probeTestDirectory) Names(limit int) ([]string, error) {
	names := make([]string, 0, len(directory.object.files)+len(directory.object.directories))
	for name := range directory.object.files {
		names = append(names, name)
	}
	for name := range directory.object.directories {
		names = append(names, name)
	}
	slices.Sort(names)
	if len(names) > limit {
		return nil, errors.New("directory entry bound exceeded")
	}
	return names, nil
}

func (directory *probeTestDirectory) OpenDirectory(name string, _ bool) (outputcap.Directory, error) {
	object, exists := directory.object.directories[name]
	if !exists {
		return nil, fs.ErrNotExist
	}
	return newProbeTestDirectory(directory.system, object), nil
}

func (directory *probeTestDirectory) CreateDirectory(name string, _ bool) (outputcap.Directory, error) {
	if _, exists := directory.object.directories[name]; exists {
		return nil, outputcap.ErrNamespaceCollision
	}
	if _, exists := directory.object.files[name]; exists {
		return nil, outputcap.ErrNamespaceCollision
	}
	if err := directory.system.event("create-directory:" + name); err != nil {
		return nil, err
	}
	object := newProbeTestDirectoryObject()
	directory.object.directories[name] = object
	return newProbeTestDirectory(directory.system, object), nil
}

func (directory *probeTestDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	candidate, ok := other.(*probeTestDirectory)
	if !ok || directory.closed || candidate.closed {
		return false, fs.ErrClosed
	}
	return directory.object == candidate.object, nil
}

func (directory *probeTestDirectory) RemoveDirectory(name string, expected outputcap.Directory) error {
	candidate := expected.(*probeTestDirectory)
	object, exists := directory.object.directories[name]
	if !exists {
		return fs.ErrNotExist
	}
	if candidate.closed || object != candidate.object {
		return outputcap.ErrUnsafeNamespace
	}
	if len(object.files)+len(object.directories) != 0 {
		return errors.New("directory is not empty")
	}
	if err := directory.system.event("remove-directory:" + name); err != nil {
		return err
	}
	delete(directory.object.directories, name)
	return nil
}

func (directory *probeTestDirectory) InstallDirectoryNoReplace(source outputcap.Directory, name string) (outputcap.Directory, error) {
	candidate := source.(*probeTestDirectory)
	if _, exists := directory.object.directories[name]; exists {
		return nil, outputcap.ErrNamespaceCollision
	}
	var sourceName string
	for entry, object := range directory.object.directories {
		if object == candidate.object {
			sourceName = entry
			break
		}
	}
	if sourceName == "" {
		return nil, outputcap.ErrUnsafeNamespace
	}
	directory.object.directories[name] = candidate.object
	delete(directory.object.directories, sourceName)
	if err := directory.system.event("installed-directory:" + name); err != nil {
		return nil, err
	}
	return newProbeTestDirectory(directory.system, candidate.object), nil
}

func (directory *probeTestDirectory) CreateFile(name string, _ bool, size int64) (outputcap.MutableFile, error) {
	if _, exists := directory.object.files[name]; exists {
		return nil, outputcap.ErrNamespaceCollision
	}
	if err := directory.system.event("create-file:" + name); err != nil {
		return nil, err
	}
	object := &probeTestFileObject{size: uint64(size)}
	directory.object.files[name] = object
	return directory.openFile(name, object), nil
}

func (directory *probeTestDirectory) OpenObservedFile(name string, _ bool) (outputcap.ObservedFile, error) {
	object, exists := directory.object.files[name]
	if !exists {
		return nil, fs.ErrNotExist
	}
	return directory.openFile(name, object), nil
}

func (directory *probeTestDirectory) openFile(name string, object *probeTestFileObject) *probeTestFile {
	directory.system.openHandles++
	return &probeTestFile{system: directory.system, object: object, name: name}
}

func (directory *probeTestDirectory) LinkFileNoReplace(source outputcap.FileIdentity, name string) (outputcap.ObservedFile, error) {
	if _, exists := directory.object.files[name]; exists {
		return nil, outputcap.ErrNamespaceCollision
	}
	file := source.(*probeTestFile)
	directory.object.files[name] = file.object
	if err := directory.system.event("linked-file:" + name); err != nil {
		return nil, err
	}
	return directory.openFile(name, file.object), nil
}

func (directory *probeTestDirectory) ReplacePrivateFile(source outputcap.FileIdentity, name string) error {
	file := source.(*probeTestFile)
	if err := directory.system.event("replace-file:" + name); err != nil {
		return err
	}
	directory.object.files[name] = file.object
	delete(directory.object.files, file.name)
	return directory.system.event("replaced-file:" + name)
}

func (directory *probeTestDirectory) RemoveFile(name string, expected outputcap.FileIdentity) error {
	file := expected.(*probeTestFile)
	object, exists := directory.object.files[name]
	if !exists {
		return fs.ErrNotExist
	}
	if file.closed || object != file.object {
		return outputcap.ErrUnsafeNamespace
	}
	if err := directory.system.event("remove-file:" + name); err != nil {
		return err
	}
	delete(directory.object.files, name)
	return nil
}

type probeTestFile struct {
	outputcap.MutableFile
	system *probeTestFilesystem
	object *probeTestFileObject
	name   string
	closed bool
}

func (file *probeTestFile) Close() error {
	if !file.closed {
		file.closed = true
		file.system.openHandles--
	}
	return nil
}

func (file *probeTestFile) Size() (uint64, error) {
	if file.closed {
		return 0, fs.ErrClosed
	}
	return file.object.size, nil
}

func (file *probeTestFile) SameFile(other outputcap.FileIdentity) (bool, error) {
	candidate, ok := other.(*probeTestFile)
	if !ok || file.closed || candidate.closed {
		return false, fs.ErrClosed
	}
	return file.object == candidate.object, nil
}

func (file *probeTestFile) Sync() error {
	if file.closed {
		return fs.ErrClosed
	}
	return file.system.event("sync-file:" + file.name)
}

func probeTestNames(root *probeTestDirectory) []string {
	names := make([]string, 0, len(root.object.directories))
	for name := range root.object.directories {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func probeTestName(index int) string { return fmt.Sprintf("%s%032x", NamePrefix, index) }
