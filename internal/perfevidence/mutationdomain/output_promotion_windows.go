//go:build windows

package mutationdomain

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/windows"
)

type windowsPromotionAuthority struct {
	path     string
	handle   windows.Handle
	identity appContainerIdentity
	closed   bool
}

type platformPromotedInput struct {
	pathname     string
	artifactLeaf string
	file         *os.File
	security     windows.Handle
	directory    windows.Handle
	identity     appContainerIdentity
	closed       bool
}

var activeWindowsPromotion struct {
	sync.Mutex
	authority *windowsPromotionAuthority
}

func openWindowsPromotionAuthority(path string) (*windowsPromotionAuthority, error) {
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		encoded,
		windows.GENERIC_ALL,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	fail := func(operationErr error) (*windowsPromotionAuthority, error) {
		return nil, errors.Join(operationErr, windows.CloseHandle(handle))
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fail(errors.Join(errors.New("promotion authority is not a no-follow directory"), err))
	}
	identity, err := currentAppContainerIdentity()
	if err != nil {
		return fail(fmt.Errorf("retain promotion AppContainer identity: %w", err))
	}
	authority := &windowsPromotionAuthority{path: path, handle: handle, identity: identity}
	activeWindowsPromotion.Lock()
	if activeWindowsPromotion.authority != nil {
		activeWindowsPromotion.Unlock()
		return fail(errors.New("Windows promotion authority is already registered"))
	}
	activeWindowsPromotion.authority = authority
	activeWindowsPromotion.Unlock()
	return authority, nil
}

func platformPromoteProtectedOutput(
	source *os.File,
	verify func() error,
	size int64,
	sha256Value string,
	mode os.FileMode,
	semanticPath string,
) (*platformPromotedInput, error) {
	if source == nil || verify == nil || size < 0 || len(sha256Value) != 64 {
		return nil, errors.New("Windows output promotion authority is invalid")
	}
	activeWindowsPromotion.Lock()
	authority := activeWindowsPromotion.authority
	activeWindowsPromotion.Unlock()
	if authority == nil || authority.closed {
		return nil, errors.New("Windows promotion root authority is unavailable")
	}
	entropy, err := randomBytes(16)
	if err != nil {
		return nil, err
	}
	generationLeaf := "generation-" + fmt.Sprintf("%x", entropy)
	mutableDescriptor, err := windows.SecurityDescriptorFromString(appContainerObjectDescriptor(
		authority.identity.traditionalUserSID,
		authority.identity.isolationCapabilitySID,
	))
	if err != nil {
		return nil, err
	}
	generation, err := createSealedObject(authority.handle, generationLeaf, true, mutableDescriptor)
	if err != nil {
		return nil, fmt.Errorf("create retained Windows output generation: %w", err)
	}
	artifactLeaf := promotedArtifactName(semanticPath)
	result := &platformPromotedInput{
		pathname: filepath.Join(authority.path, generationLeaf, artifactLeaf), artifactLeaf: artifactLeaf,
		directory: generation, identity: authority.identity,
	}
	fail := func(operationErr error) (*platformPromotedInput, error) {
		return nil, errors.Join(operationErr, result.close())
	}
	if _, err := source.Seek(0, 0); err != nil {
		return fail(err)
	}
	artifact, observedSHA256, err := copySealedFile(source, generation, artifactLeaf, sealedObjectCreator{
		descriptor: mutableDescriptor,
	}, true)
	if err != nil {
		return fail(fmt.Errorf("copy retained Windows output generation: %w", err))
	}
	result.file = artifact
	information, statErr := artifact.Stat()
	verificationErr := verify()
	if statErr != nil || information.Size() != size || observedSHA256 != sha256Value {
		return fail(errors.Join(
			fmt.Errorf(
				"promoted Windows output identity is bytes=%d sha256=%s, want bytes=%d sha256=%s",
				informationSize(information), observedSHA256, size, sha256Value,
			),
			statErr,
			verificationErr,
		))
	}
	if verificationErr != nil {
		return fail(fmt.Errorf("verify retained Windows output after promotion: %w", verificationErr))
	}
	if err := artifact.Chmod(mode.Perm()); err != nil {
		return fail(err)
	}
	if _, err := artifact.Seek(0, 0); err != nil {
		return fail(err)
	}
	readOnlyDescriptor := appContainerReadOnlyObjectDescriptor(
		authority.identity.traditionalUserSID,
		authority.identity.isolationCapabilitySID,
	)
	result.file = nil
	retained, securityAuthority, err := finalizeWindowsExecutableFile(
		artifact,
		generation,
		artifactLeaf,
		result.pathname,
		readOnlyDescriptor,
		readOnlyDescriptor,
	)
	if err != nil {
		return fail(fmt.Errorf("freeze retained Windows output generation: %w", err))
	}
	result.file = retained
	result.security = securityAuthority
	return result, nil
}

func informationSize(information os.FileInfo) int64 {
	if information == nil {
		return -1
	}
	return information.Size()
}

func (input *platformPromotedInput) pathValue() string {
	if input == nil {
		return ""
	}
	return input.pathname
}

func (input *platformPromotedInput) path() string {
	return input.pathValue()
}

func (input *platformPromotedInput) close() error {
	if input == nil || input.closed {
		return nil
	}
	input.closed = true
	var fileErrs []error
	if input.security != 0 && input.security != windows.InvalidHandle && input.identity.traditionalUserSID != nil {
		if err := sealWindowsHandleDACL(input.security, appContainerObjectDescriptor(
			input.identity.traditionalUserSID,
			input.identity.isolationCapabilitySID,
		)); err != nil {
			fileErrs = append(fileErrs, fmt.Errorf("enter promoted artifact teardown phase: %w", err))
		}
	}
	if input.file != nil {
		if input.security == 0 || input.security == windows.InvalidHandle {
			fileErrs = append(fileErrs, markWindowsHandleForDeletion(windows.Handle(input.file.Fd())))
		}
		fileErrs = append(fileErrs, input.file.Close())
		input.file = nil
	}
	if input.security != 0 && input.security != windows.InvalidHandle {
		artifact, err := openWindowsObjectForDeletion(input.directory, input.artifactLeaf, false)
		if err == nil {
			err = errors.Join(markWindowsHandleForDeletion(artifact), windows.CloseHandle(artifact))
		}
		if err != nil {
			fileErrs = append(fileErrs, fmt.Errorf("unlink retained promoted artifact: %w", err))
		}
		fileErrs = append(fileErrs, windows.CloseHandle(input.security))
		input.security = 0
	}
	var directoryErr error
	if input.directory != 0 && input.directory != windows.InvalidHandle {
		directoryErr = errors.Join(
			markWindowsHandleForDeletion(input.directory),
			windows.CloseHandle(input.directory),
		)
		input.directory = 0
	}
	return errors.Join(errors.Join(fileErrs...), directoryErr)
}

func (authority *windowsPromotionAuthority) close() error {
	if authority == nil || authority.closed {
		return nil
	}
	authority.closed = true
	activeWindowsPromotion.Lock()
	if activeWindowsPromotion.authority == authority {
		activeWindowsPromotion.authority = nil
	}
	activeWindowsPromotion.Unlock()
	closeErr := windows.CloseHandle(authority.handle)
	authority.handle = 0
	// SID.Copy stores its bytes in Go-managed memory; clearing the retained pointer
	// releases ownership without calling the native FreeSid allocator.
	authority.identity = appContainerIdentity{}
	return closeErr
}
