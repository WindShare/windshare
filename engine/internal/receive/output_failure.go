package receive

import "github.com/windshare/windshare/core/osfs"

// The native adapter is the authority for filesystem diagnostics. Keeping the
// carrier private prevents an arbitrary provider error from forging its class.
type filesystemFailure struct {
	cause      error
	diagnostic osfs.FilesystemOutputDiagnostic
}

func (f *filesystemFailure) Error() string { return f.cause.Error() }
func (f *filesystemFailure) Unwrap() error { return f.cause }
func sealFilesystemOutputFailure(cause error) error {
	if cause == nil {
		return nil
	}
	diagnostic, ok := osfs.FilesystemOutputDiagnosticFor(cause)
	if !ok {
		return cause
	}
	if !diagnostic.Valid() {
		return errGetOutputAdapterContract
	}
	return &filesystemFailure{cause: cause, diagnostic: diagnostic}
}
func OutputDiagnostic(cause error) (osfs.FilesystemOutputDiagnostic, bool) {
	//nolint:errorlint
	failure, ok := cause.(*filesystemFailure)
	if !ok || failure == nil {
		return osfs.FilesystemOutputDiagnostic{}, false
	}
	return failure.diagnostic, failure.diagnostic.Valid()
}
