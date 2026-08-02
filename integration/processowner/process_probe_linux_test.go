//go:build linux

package processowner_test

import (
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

type processProbe struct {
	descriptor int
}

func retainProcessProbe(processID int) (*processProbe, error) {
	if processID < 1 {
		return nil, errors.New("process probe PID is invalid")
	}
	descriptor, err := unix.PidfdOpen(processID, 0)
	if err != nil {
		return nil, err
	}
	return &processProbe{descriptor: descriptor}, nil
}

func (probe *processProbe) waitRetired(timeout time.Duration) error {
	if probe == nil || probe.descriptor < 0 || timeout <= 0 || timeout.Milliseconds() > int64(^uint32(0)>>1) {
		return errors.New("process probe wait authority is invalid")
	}
	descriptors := []unix.PollFd{{Fd: int32(probe.descriptor), Events: unix.POLLIN}}
	count, err := unix.Poll(descriptors, int(timeout.Milliseconds()))
	if err != nil {
		return err
	}
	if count != 1 || descriptors[0].Revents&(unix.POLLIN|unix.POLLHUP) == 0 {
		return errors.New("process remained active beyond the probe deadline")
	}
	return nil
}

func (probe *processProbe) close() {
	if probe != nil && probe.descriptor >= 0 {
		_ = unix.Close(probe.descriptor)
		probe.descriptor = -1
	}
}
