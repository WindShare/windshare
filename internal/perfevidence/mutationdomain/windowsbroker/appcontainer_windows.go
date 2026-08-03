//go:build windows

package windowsbroker

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type appContainerAuthority struct {
	profileName     string
	profileMarker   string
	traditionalSID  *windows.SID
	packageSID      *windows.SID
	capabilitySID   *windows.SID
	descriptor      string
	rootPath        string
	root            windows.Handle
	helperPath      string
	helperLeaf      string
	helperDirectory windows.Handle
	helperSecurity  windows.Handle
	helper          *os.File
	manifestPath    string
}

type sealedObjectCreator struct {
	token           windows.Token
	descriptor      *windows.SECURITY_DESCRIPTOR
	finalDescriptor string
}

func createPrivateAppContainer(
	configuration initialization,
	retainedImage *os.File,
	stageInputs StageInputs,
) (*appContainerAuthority, error) {
	profileEntropy, err := randomBytes(appContainerProfileEntropyBytes)
	if err != nil {
		return nil, err
	}
	profileName := appContainerProfilePrefix + hex.EncodeToString(profileEntropy)
	profileMarker, err := createAppContainerRecoveryMarker(configuration.RuntimeRoot, profileName)
	if err != nil {
		return nil, err
	}
	authority := &appContainerAuthority{profileName: profileName, profileMarker: profileMarker}
	fail := func(operationErr error) (*appContainerAuthority, error) {
		return nil, errors.Join(operationErr, authority.close())
	}
	packageSID, err := createEphemeralAppContainerProfile(profileName)
	if err != nil {
		return fail(err)
	}
	authority.packageSID = packageSID
	authority.traditionalSID, err = tokenUserSID(windows.GetCurrentProcessToken())
	if err != nil {
		return fail(fmt.Errorf("retain trusted Windows user SID: %w", err))
	}
	authority.capabilitySID, err = newIsolationCapabilitySID()
	if err != nil {
		return fail(fmt.Errorf("derive private AppContainer isolation capability: %w", err))
	}
	authority.descriptor = appContainerObjectDescriptor(authority.traditionalSID, authority.capabilitySID)
	creationDescriptorText, err := hostObjectCreationDescriptor()
	if err != nil {
		return fail(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(creationDescriptorText)
	if err != nil {
		return fail(err)
	}
	rootCreator := sealedObjectCreator{descriptor: descriptor}
	creator := sealedObjectCreator{descriptor: descriptor, finalDescriptor: authority.descriptor}
	rootEntropy, err := randomBytes(16)
	if err != nil {
		return fail(err)
	}
	profileRoot, err := appContainerFolderPath(authority.packageSID)
	if err != nil {
		return fail(err)
	}
	authority.rootPath = filepath.Join(
		profileRoot, privateRootDirectory+"-"+hex.EncodeToString(rootEntropy),
	)
	authority.root, err = rootCreator.create(0, windowsNTPath(authority.rootPath), true)
	if err != nil {
		return fail(fmt.Errorf("atomically create private AppContainer root: %w", err))
	}
	if stageInputs == nil {
		return fail(errors.New("sealed AppContainer input stager is unavailable"))
	}
	authority.manifestPath, err = stageInputs(authority.rootPath, authority.root, configuration.Roots, creator.create)
	if err != nil {
		return fail(fmt.Errorf("stage sealed AppContainer inputs: %w", err))
	}
	if err := sealWindowsNamedDACL(authority.root, authority.descriptor); err != nil {
		return fail(fmt.Errorf("seal private AppContainer root DACL: %w", err))
	}
	if _, err := retainedImage.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	helperEntropy, err := randomBytes(16)
	if err != nil {
		return fail(err)
	}
	helperDirectoryPath := filepath.Join(profileRoot, "mutation-helper-"+hex.EncodeToString(helperEntropy))
	authority.helperDirectory, err = rootCreator.create(0, windowsNTPath(helperDirectoryPath), true)
	if err != nil {
		return fail(fmt.Errorf("create retained AppContainer helper directory: %w", err))
	}
	authority.helperLeaf = "helper.exe"
	authority.helperPath = filepath.Join(helperDirectoryPath, authority.helperLeaf)
	writableHelper, _, err := copySealedFile(
		retainedImage, authority.helperDirectory, authority.helperLeaf, rootCreator.create, true,
	)
	if err != nil {
		return fail(fmt.Errorf("create retained AppContainer helper image: %w", err))
	}
	authority.helper, authority.helperSecurity, err = finalizeWindowsHelperImage(
		writableHelper,
		authority.helperDirectory,
		authority.helperLeaf,
		authority.helperPath,
		authority.traditionalSID,
		authority.capabilitySID,
	)
	if err != nil {
		return fail(fmt.Errorf("seal retained AppContainer helper image: %w", err))
	}
	return authority, nil
}

func appContainerFolderPath(packageSID *windows.SID) (string, error) {
	encodedSID, err := windows.UTF16PtrFromString(packageSID.String())
	if err != nil {
		return "", err
	}
	var folder *uint16
	result, _, _ := getAppContainerFolderPath.Call(
		uintptr(unsafe.Pointer(encodedSID)), uintptr(unsafe.Pointer(&folder)),
	)
	if int32(result) < 0 || folder == nil {
		return "", fmt.Errorf("resolve AppContainer storage: HRESULT 0x%08x", uint32(result))
	}
	path := windows.UTF16PtrToString(folder)
	windows.CoTaskMemFree(unsafe.Pointer(folder))
	return path, nil
}

func appContainerObjectDescriptor(traditionalUserSID, capabilitySID *windows.SID) string {
	// Windows evaluates AppContainer access twice: the trusted invoking user must
	// pass the traditional check, and the fresh capability must independently pass
	// the restricted check. No ambient package or network capability is admitted.
	return fmt.Sprintf(
		"D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)S:(ML;OICI;NW;;;LW)",
		traditionalUserSID.String(),
		capabilitySID.String(),
	)
}

func appContainerReadOnlyObjectDescriptor(traditionalUserSID, capabilitySID *windows.SID) string {
	return fmt.Sprintf(
		"D:P(A;OICI;GRGX;;;%s)(A;OICI;GRGX;;;%s)(A;OICI;FA;;;SY)S:(ML;OICI;NW;;;LW)",
		traditionalUserSID.String(),
		capabilitySID.String(),
	)
}

func hostObjectCreationDescriptor() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)S:(ML;OICI;NW;;;LW)",
		user.User.Sid.String(),
	), nil
}

func appContainerHelperDescriptors(traditionalUserSID, capabilitySID *windows.SID) (file, directory string, err error) {
	trustedUserText := traditionalUserSID.String()
	capabilityText := capabilitySID.String()
	// The broker must be able to traverse and read the image because CreateProcess
	// opens it before constructing the AppContainer token. The trusted user and
	// private capability receive only read/execute; neither can rewrite its ACL.
	file = fmt.Sprintf(
		"D:P(D;;WDWO;;;OW)(A;;GRGX;;;%s)(A;;GRGX;;;%s)(A;;FA;;;SY)S:(ML;;NW;;;LW)",
		trustedUserText,
		capabilityText,
	)
	directory = fmt.Sprintf(
		"D:P(D;OICI;WDWO;;;OW)(A;OICI;GRGX;;;%s)(A;OICI;GRGX;;;%s)(A;OICI;FA;;;SY)S:(ML;OICI;NW;;;LW)",
		trustedUserText,
		capabilityText,
	)
	return file, directory, nil
}

func appContainerHelperTeardownDescriptor() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	// This descriptor is installed only after the helper Job reports zero active
	// processes and the broker has closed its pinned image handle. Avoiding
	// inheritable ACEs prevents teardown from rewriting the sealed child DACL.
	return fmt.Sprintf(
		"D:P(D;;WDWO;;;OW)(A;;GRGX;;;%s)(A;;DC;;;%s)(A;;FA;;;SY)S:(ML;;NW;;;LW)",
		user.User.Sid.String(),
		user.User.Sid.String(),
	), nil
}

func appContainerHelperFileTeardownDescriptor() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"D:P(A;;GRSD;;;%s)(A;;FA;;;SY)S:(ML;;NW;;;LW)",
		user.User.Sid.String(),
	), nil
}

func finalizeWindowsHelperImage(
	writable *os.File,
	directory windows.Handle,
	leaf string,
	path string,
	traditionalUserSID *windows.SID,
	capabilitySID *windows.SID,
) (*os.File, windows.Handle, error) {
	fileDescriptor, directoryDescriptor, err := appContainerHelperDescriptors(traditionalUserSID, capabilitySID)
	if err != nil {
		return nil, windows.InvalidHandle, errors.Join(err, writable.Close())
	}
	return finalizeWindowsExecutableFile(writable, directory, leaf, path, fileDescriptor, directoryDescriptor)
}

func finalizeWindowsExecutableFile(
	writable *os.File,
	directory windows.Handle,
	leaf string,
	path string,
	fileDescriptor string,
	directoryDescriptor string,
) (*os.File, windows.Handle, error) {
	securityAuthority, securityInformation, err := openWindowsFileSecurityAuthority(directory, leaf)
	if err != nil {
		return nil, windows.InvalidHandle, errors.Join(fmt.Errorf("retain executable security authority: %w", err), writable.Close())
	}
	var writableInformation windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(writable.Fd()), &writableInformation); err != nil ||
		!sameWindowsObject(writableInformation, securityInformation) {
		return nil, windows.InvalidHandle, errors.Join(
			errors.New("Windows executable security authority does not identify the copied image"),
			err,
			windows.CloseHandle(securityAuthority),
			writable.Close(),
		)
	}
	if err := sealWindowsHandleDACL(securityAuthority, fileDescriptor); err != nil {
		return nil, windows.InvalidHandle, errors.Join(fmt.Errorf("seal executable file DACL: %w", err), windows.CloseHandle(securityAuthority), writable.Close())
	}
	if err := sealWindowsHandleDACL(directory, directoryDescriptor); err != nil {
		return nil, windows.InvalidHandle, errors.Join(fmt.Errorf("seal executable directory DACL: %w", err), windows.CloseHandle(securityAuthority), writable.Close())
	}
	intermediate, intermediateInfo, err := openRetainedWindowsFile(
		directory,
		leaf,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
	)
	if err != nil {
		return nil, windows.InvalidHandle, errors.Join(fmt.Errorf("open intermediate executable read authority: %w", err), windows.CloseHandle(securityAuthority), writable.Close())
	}
	if err := windows.GetFileInformationByHandle(windows.Handle(writable.Fd()), &writableInformation); err != nil ||
		!sameWindowsObject(writableInformation, intermediateInfo) {
		return nil, windows.InvalidHandle, errors.Join(
			errors.New("retained Windows executable identity changed before write authority was released"),
			err,
			windows.CloseHandle(intermediate),
			windows.CloseHandle(securityAuthority),
			writable.Close(),
		)
	}
	if err := writable.Close(); err != nil {
		return nil, windows.InvalidHandle, errors.Join(fmt.Errorf("release executable write authority: %w", err), windows.CloseHandle(intermediate), windows.CloseHandle(securityAuthority))
	}
	retained, retainedInfo, err := openRetainedWindowsFile(directory, leaf, windows.FILE_SHARE_READ)
	if err != nil {
		return nil, windows.InvalidHandle, errors.Join(fmt.Errorf("open final executable read authority: %w", err), windows.CloseHandle(intermediate), windows.CloseHandle(securityAuthority))
	}
	if !sameWindowsObject(intermediateInfo, retainedInfo) {
		return nil, windows.InvalidHandle, errors.Join(
			errors.New("retained Windows executable identity changed while sealing share access"),
			windows.CloseHandle(retained),
			windows.CloseHandle(intermediate),
			windows.CloseHandle(securityAuthority),
		)
	}
	if err := windows.CloseHandle(intermediate); err != nil {
		return nil, windows.InvalidHandle, errors.Join(err, windows.CloseHandle(retained), windows.CloseHandle(securityAuthority))
	}
	return os.NewFile(uintptr(retained), path), securityAuthority, nil
}

func openWindowsFileSecurityAuthority(
	root windows.Handle,
	leaf string,
) (windows.Handle, windows.ByHandleFileInformation, error) {
	name, err := windows.NewNTUnicodeString(leaf)
	if err != nil {
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: root,
		ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle,
		windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		attributes,
		&status,
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	)
	if err != nil {
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil ||
		information.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 ||
		information.NumberOfLinks != 1 {
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, errors.Join(
			errors.New("Windows file security authority is not a single-link no-follow regular file"),
			err,
			windows.CloseHandle(handle),
		)
	}
	return handle, information, nil
}

func openRetainedWindowsFile(
	root windows.Handle,
	leaf string,
	share uint32,
) (windows.Handle, windows.ByHandleFileInformation, error) {
	name, err := windows.NewNTUnicodeString(leaf)
	if err != nil {
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: root,
		ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	desired := uint32(
		windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES | windows.FILE_READ_EA |
			windows.FILE_EXECUTE | windows.SYNCHRONIZE,
	)
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle,
		desired,
		attributes,
		&status,
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		share,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	)
	if err != nil {
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil ||
		information.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 ||
		information.NumberOfLinks != 1 {
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, errors.Join(
			errors.New("retained helper image is not a single-link no-follow regular file"),
			err,
			windows.CloseHandle(handle),
		)
	}
	return handle, information, nil
}

func appContainerProcessDescriptor(traditionalUserSID, capabilitySID *windows.SID) string {
	const processQueryAndSynchronize = "0x00121000"
	return fmt.Sprintf(
		"D:P(A;;%s;;;%s)(A;;%s;;;%s)(A;;GA;;;SY)",
		processQueryAndSynchronize,
		traditionalUserSID.String(),
		processQueryAndSynchronize,
		capabilitySID.String(),
	)
}

func appContainerThreadDescriptor(traditionalUserSID, capabilitySID *windows.SID) string {
	const threadQueryAndSynchronize = "0x00120800"
	return fmt.Sprintf(
		"D:P(A;;%s;;;%s)(A;;%s;;;%s)(A;;GA;;;SY)",
		threadQueryAndSynchronize,
		traditionalUserSID.String(),
		threadQueryAndSynchronize,
		capabilitySID.String(),
	)
}

func hostProcessCreationDescriptor() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("D:P(A;;GA;;;%s)(A;;GA;;;SY)", user.User.Sid.String()), nil
}

func sealWindowsKernelHandleDACL(handle windows.Handle, descriptorText string) error {
	descriptor, err := windows.SecurityDescriptorFromString(descriptorText)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(
		handle,
		windows.SE_KERNEL_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	)
}

func sealWindowsHandleDACL(handle windows.Handle, descriptorText string) error {
	if handle == 0 || handle == windows.InvalidHandle {
		return errors.New("DACL authority handle is unavailable")
	}
	descriptor, err := windows.SecurityDescriptorFromString(descriptorText)
	if err != nil {
		return err
	}
	status, _, _ := ntSetSecurityObject.Call(
		uintptr(handle),
		uintptr(windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION),
		uintptr(unsafe.Pointer(descriptor)),
	)
	if int32(status) < 0 {
		return windows.NTStatus(uint32(status))
	}
	return nil
}

func randomBytes(count int) ([]byte, error) {
	content := make([]byte, count)
	if _, err := rand.Read(content); err != nil {
		return nil, err
	}
	return content, nil
}

const (
	appContainerProfilePrefix          = "WindShare.Performance."
	appContainerProfileEntropyBytes    = 16
	appContainerProfileEntropyHexBytes = appContainerProfileEntropyBytes * 2
	appContainerLedgerDirectory        = ".windshare-appcontainer-profiles"
	appContainerLedgerLock             = "ledger.lock"
	appContainerMarkerSuffix           = ".pending"
	appContainerMarkerMaximumBytes     = 256
	appContainerLedgerMaximumEntries   = 1024
	appContainerLedgerReadBatch        = 64
	hresultFileNotFound                = 0x80070002
	hresultNotFound                    = 0x80070490
)

func createAppContainerRecoveryMarker(runtimeRoot, profileName string) (string, error) {
	if !validEphemeralAppContainerProfileName(profileName) {
		return "", fmt.Errorf("refuse recovery marker for unreserved AppContainer profile %q", profileName)
	}
	directory := filepath.Join(runtimeRoot, appContainerLedgerDirectory)
	marker := filepath.Join(directory, profileName+appContainerMarkerSuffix)
	file, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create AppContainer recovery marker: %w", err)
	}
	_, writeErr := file.WriteString(profileName + "\n")
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return "", errors.Join(err, os.Remove(marker))
	}
	return marker, nil
}

func createEphemeralAppContainerProfile(profileName string) (*windows.SID, error) {
	if !validEphemeralAppContainerProfileName(profileName) {
		return nil, fmt.Errorf("refuse creation of unreserved AppContainer profile %q", profileName)
	}
	encodedName, err := windows.UTF16PtrFromString(profileName)
	if err != nil {
		return nil, err
	}
	display, _ := windows.UTF16PtrFromString("WindShare private performance mutation domain")
	description, _ := windows.UTF16PtrFromString("Ephemeral no-network performance evidence helper")
	var packageSID *windows.SID
	result, _, _ := createAppContainerProfile.Call(
		uintptr(unsafe.Pointer(encodedName)),
		uintptr(unsafe.Pointer(display)),
		uintptr(unsafe.Pointer(description)),
		0,
		0,
		uintptr(unsafe.Pointer(&packageSID)),
	)
	if int32(result) < 0 {
		return nil, fmt.Errorf("create AppContainer profile: HRESULT 0x%08x", uint32(result))
	}
	if packageSID == nil {
		return nil, errors.New("created AppContainer profile returned no package SID")
	}
	return packageSID, nil
}

func releaseNativeAppContainerSID(packageSID *windows.SID) error {
	if packageSID == nil {
		return nil
	}
	// CreateAppContainerProfile transfers a native SID allocation to its caller.
	// Token SID copies use Go memory and deliberately never enter this function.
	return windows.FreeSid(packageSID)
}

func deleteEphemeralAppContainerProfile(profileName string) error {
	if !validEphemeralAppContainerProfileName(profileName) {
		return fmt.Errorf("refuse deletion of unreserved AppContainer profile %q", profileName)
	}
	encoded, err := windows.UTF16PtrFromString(profileName)
	if err != nil {
		return err
	}
	result, _, _ := deleteAppContainerProfile.Call(uintptr(unsafe.Pointer(encoded)))
	hresult := uint32(result)
	if int32(result) >= 0 || hresult == hresultFileNotFound || hresult == hresultNotFound {
		return nil
	}
	return fmt.Errorf("delete AppContainer profile: HRESULT 0x%08x", hresult)
}

func validEphemeralAppContainerProfileName(profileName string) bool {
	if !strings.HasPrefix(profileName, appContainerProfilePrefix) ||
		len(profileName) != len(appContainerProfilePrefix)+appContainerProfileEntropyHexBytes {
		return false
	}
	for _, character := range profileName[len(appContainerProfilePrefix):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
