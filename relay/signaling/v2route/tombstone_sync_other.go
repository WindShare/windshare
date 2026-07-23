//go:build !windows && !linux && !darwin

package v2route

func syncDirectory(string) error { return nil }
