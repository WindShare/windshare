//go:build windows

package outputruntime

import "golang.org/x/sys/windows"

func runtimeMakePrivateEnvelopeUnsafe(path string) error {
	descriptor, err := windows.SecurityDescriptorFromString("D:(A;;GA;;;WD)")
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		return err
	}
	markPortableRuntimePrivateEnvelopeUnsafe(path)
	return nil
}
