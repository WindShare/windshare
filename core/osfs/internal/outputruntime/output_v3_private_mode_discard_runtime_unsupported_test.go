//go:build !linux && !windows

package outputruntime

import "errors"

func runtimeMakePrivateEnvelopeUnsafe(string) error {
	return errors.New("private output envelope testing is unsupported on this platform")
}
