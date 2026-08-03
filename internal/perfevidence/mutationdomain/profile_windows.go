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

	"github.com/windshare/windshare/internal/perfevidence/mutationdomain/windowsbroker"
	"golang.org/x/sys/windows"
)

const (
	appContainerProfilePrefix          = windowsbroker.ProfileNamePrefix
	appContainerProfileEntropyBytes    = windowsbroker.ProfileEntropyBytes
	appContainerProfileEntropyHexBytes = windowsbroker.ProfileEntropyHexBytes
	appContainerLedgerDirectory        = ".windshare-appcontainer-profiles"
	appContainerLedgerLock             = "ledger.lock"
	appContainerMarkerSuffix           = ".pending"
	appContainerMarkerMaximumBytes     = 256
	appContainerLedgerMaximumEntries   = 1024
	appContainerLedgerReadBatch        = 64
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
	return windowsbroker.CreateRecoveryMarker(runtimeRoot, profileName)
}

func createEphemeralAppContainerProfile(profileName string) (*windows.SID, error) {
	return windowsbroker.CreateProfile(profileName)
}

func releaseNativeAppContainerSID(packageSID *windows.SID) error {
	return windowsbroker.ReleaseProfileSID(packageSID)
}

func deleteEphemeralAppContainerProfile(profileName string) error {
	return windowsbroker.DeleteProfile(profileName)
}

func validEphemeralAppContainerProfileName(profileName string) bool {
	return windowsbroker.ValidProfileName(profileName)
}

func profileNameFromMarker(marker string) (string, bool) {
	if !strings.HasSuffix(marker, appContainerMarkerSuffix) {
		return "", false
	}
	profileName := strings.TrimSuffix(marker, appContainerMarkerSuffix)
	return profileName, validEphemeralAppContainerProfileName(profileName)
}
