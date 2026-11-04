//go:build !windows

package platformsetup

import "context"

type unsupportedPlatform struct{}

func installationExecutable() (string, error) { return "", ErrUnsupported }

func nativePlatform() unsupportedPlatform { return unsupportedPlatform{} }
func (unsupportedPlatform) Inspect(context.Context, string) (Snapshot, error) {
	return Snapshot{}, ErrUnsupported
}
func (unsupportedPlatform) Apply(context.Context, string, RuleAction, bool) Operation {
	return Operation{State: OperationUnsupported, Detail: ErrUnsupported.Error()}
}
