package liveshare

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/catalog"
)

// DirectoryScanAdmission lets the sender assembly impose request-scoped
// admission before filesystem enumeration. It intentionally receives the
// immutable catalog request but no filesystem source authority.
type DirectoryScanAdmission interface {
	AdmitDirectoryScan(context.Context, catalog.ScanRequest) error
}

type DirectoryScanAdmissionFunc func(context.Context, catalog.ScanRequest) error

func (admit DirectoryScanAdmissionFunc) AdmitDirectoryScan(
	ctx context.Context,
	request catalog.ScanRequest,
) error {
	if admit == nil {
		return errors.New("live share directory scan admission is nil")
	}
	return admit(ctx, request)
}

func directoryScannerWithAdmission(
	scanner catalog.DirectoryScanner,
	admission DirectoryScanAdmission,
) catalog.DirectoryScanner {
	if admission == nil {
		return scanner
	}
	return catalog.DirectoryScannerFunc(func(
		ctx context.Context,
		request catalog.ScanRequest,
	) (catalog.ScanResult, error) {
		if err := admission.AdmitDirectoryScan(ctx, request); err != nil {
			return catalog.ScanResult{}, err
		}
		return scanner.ScanDirectory(ctx, request)
	})
}
