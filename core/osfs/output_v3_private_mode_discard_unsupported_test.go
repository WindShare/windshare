//go:build !linux && !windows

package osfs

import "errors"

func v3RecoveryMakePrivateEnvelopeUnsafe(string) error {
	return errors.New("private output envelope testing is unsupported on this platform")
}
