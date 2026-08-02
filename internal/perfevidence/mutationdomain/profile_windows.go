//go:build windows

package mutationdomain

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

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

type windowsProfileLedger struct {
	directory     string
	lock          windows.Handle
	deleteProfile func(string) error
	closed        bool
}

func openWindowsProfileLedger(runtimeRoot string) (*windowsProfileLedger, error) {
	if runtimeRoot == "" {
		return nil, errors.New("AppContainer profile ledger runtime root is empty")
	}
	directory := filepath.Join(runtimeRoot, appContainerLedgerDirectory)
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create AppContainer profile ledger: %w", err)
	}
	lockPath, err := windows.UTF16PtrFromString(filepath.Join(directory, appContainerLedgerLock))
	if err != nil {
		return nil, err
	}
	lock, err := windows.CreateFile(
		lockPath,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.SYNCHRONIZE,
		0,
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_HIDDEN|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("acquire exclusive AppContainer profile ledger: %w", err)
	}
	ledger := &windowsProfileLedger{
		directory:     directory,
		lock:          lock,
		deleteProfile: deleteEphemeralAppContainerProfile,
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(lock, &information); err != nil ||
		information.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 ||
		information.NumberOfLinks != 1 {
		return nil, errors.Join(
			errors.New("AppContainer profile ledger lock is not a single-link no-follow regular file"),
			err,
			ledger.close(),
		)
	}
	return ledger, nil
}

func (ledger *windowsProfileLedger) recover() error {
	if ledger == nil || ledger.closed || ledger.lock == 0 || ledger.lock == windows.InvalidHandle {
		return errors.New("AppContainer profile ledger is not exclusively retained")
	}
	directory, err := os.Open(ledger.directory)
	if err != nil {
		return fmt.Errorf("enumerate AppContainer profile ledger: %w", err)
	}
	var entries []os.DirEntry
	for {
		batch, readErr := directory.ReadDir(appContainerLedgerReadBatch)
		if len(entries) > appContainerLedgerMaximumEntries-len(batch) {
			return errors.Join(errors.New("AppContainer profile ledger exceeded its entry bound"), directory.Close())
		}
		entries = append(entries, batch...)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return errors.Join(fmt.Errorf("enumerate AppContainer profile ledger: %w", readErr), directory.Close())
		}
	}
	if err := directory.Close(); err != nil {
		return err
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
	for _, entry := range entries {
		if entry.Name() == appContainerLedgerLock {
			continue
		}
		profileName, valid := profileNameFromMarker(entry.Name())
		if !valid || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("AppContainer profile ledger contains unrecognized entry %q", entry.Name())
		}
		marker := filepath.Join(ledger.directory, entry.Name())
		content, err := os.ReadFile(marker)
		if err != nil {
			return fmt.Errorf("read AppContainer recovery marker %s: %w", entry.Name(), err)
		}
		if len(content) > appContainerMarkerMaximumBytes || string(content) != profileName+"\n" {
			return fmt.Errorf("AppContainer recovery marker %s has invalid content", entry.Name())
		}
		slog.Debug(
			"recovering abandoned AppContainer profile",
			"component", "mutationdomain",
			"operation", "appcontainer_profile_recovery",
			"profile", profileName,
		)
		deleteProfile := ledger.deleteProfile
		if deleteProfile == nil {
			deleteProfile = deleteEphemeralAppContainerProfile
		}
		if err := deleteProfile(profileName); err != nil {
			return fmt.Errorf("recover abandoned AppContainer profile %s: %w", profileName, err)
		}
		if err := os.Remove(marker); err != nil {
			return fmt.Errorf("remove recovered AppContainer profile marker %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func (ledger *windowsProfileLedger) close() error {
	if ledger == nil || ledger.closed {
		return nil
	}
	ledger.closed = true
	if ledger.lock == 0 || ledger.lock == windows.InvalidHandle {
		return nil
	}
	err := windows.CloseHandle(ledger.lock)
	ledger.lock = 0
	return err
}

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

func profileNameFromMarker(marker string) (string, bool) {
	if !strings.HasSuffix(marker, appContainerMarkerSuffix) {
		return "", false
	}
	profileName := strings.TrimSuffix(marker, appContainerMarkerSuffix)
	return profileName, validEphemeralAppContainerProfileName(profileName)
}
