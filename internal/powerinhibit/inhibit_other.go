//go:build !windows && !darwin

package powerinhibit

func acquireSystemSleepInhibition(_ string) (func(), error) {
	return func() {}, nil
}
