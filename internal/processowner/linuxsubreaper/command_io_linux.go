//go:build linux

package linuxsubreaper

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
	"github.com/windshare/windshare/internal/testrun"
	"github.com/windshare/windshare/internal/testtrace"
	"golang.org/x/sys/unix"
)

const maximumInputAbortWait = time.Second

func streamChildInput(
	source io.Reader,
	destination *os.File,
	authority *ownerprotocol.Stdin,
) error {
	defer destination.Close()
	return transferExactChildInput(source, destination, authority)
}

// A known-unstarted target still owns a request-bound input transport. Consuming
// that frame before settlement keeps target launch evidence orthogonal to whether
// the client happened to fill the pipe before preflight failed.
func drainUnstartedChildInput(source *os.File, authority *ownerprotocol.Stdin) error {
	if source == nil {
		return errors.New("known-unstarted target omitted its raw-input capability")
	}
	if err := unix.SetNonblock(int(source.Fd()), true); err != nil {
		return errors.Join(fmt.Errorf("make raw-input drain cancellable: %w", err), source.Close())
	}
	reader := boundedPipeReader{
		descriptor: int(source.Fd()),
		deadline:   time.Now().Add(maximumInputAbortWait),
	}
	return errors.Join(
		transferExactChildInput(&reader, io.Discard, authority),
		source.Close(),
	)
}

type boundedPipeReader struct {
	descriptor int
	deadline   time.Time
}

func (reader *boundedPipeReader) Read(buffer []byte) (int, error) {
	for {
		readBytes, err := unix.Read(reader.descriptor, buffer)
		switch {
		case readBytes > 0:
			return readBytes, nil
		case err == nil:
			return 0, io.EOF
		case errors.Is(err, unix.EINTR):
			continue
		case !errors.Is(err, unix.EAGAIN):
			return 0, err
		}
		remaining := time.Until(reader.deadline)
		if remaining <= 0 {
			return 0, os.ErrDeadlineExceeded
		}
		waitMilliseconds := max(int((remaining+time.Millisecond-1)/time.Millisecond), 1)
		poll := []unix.PollFd{{
			Fd: int32(reader.descriptor), Events: unix.POLLIN | unix.POLLHUP,
		}}
		count, pollErr := unix.Poll(poll, waitMilliseconds)
		switch {
		case errors.Is(pollErr, unix.EINTR):
			continue
		case pollErr != nil:
			return 0, pollErr
		case count == 0:
			return 0, os.ErrDeadlineExceeded
		case poll[0].Revents&unix.POLLNVAL != 0:
			return 0, errors.New("raw-input drain descriptor became invalid")
		}
	}
}

func transferExactChildInput(
	source io.Reader,
	destination io.Writer,
	authority *ownerprotocol.Stdin,
) error {
	byteLength := int64(0)
	if authority != nil {
		byteLength = authority.ByteLength
	}
	buffer := make([]byte, 32*1024)
	defer func() {
		for index := range buffer {
			buffer[index] = 0
		}
	}()
	remaining := byteLength
	for remaining > 0 {
		chunk := min(remaining, int64(len(buffer)))
		readBytes, readErr := io.ReadFull(source, buffer[:chunk])
		if readErr != nil {
			return fmt.Errorf("read exact child stdin bytes: %w", readErr)
		}
		for offset := 0; offset < readBytes; {
			written, writeErr := destination.Write(buffer[offset:readBytes])
			if writeErr != nil {
				return fmt.Errorf("write exact child stdin bytes: %w", writeErr)
			}
			if written < 1 {
				return io.ErrShortWrite
			}
			offset += written
		}
		for index := range readBytes {
			buffer[index] = 0
		}
		remaining -= int64(readBytes)
	}
	extra := make([]byte, 1)
	count, readErr := source.Read(extra)
	extra[0] = 0
	switch {
	case count != 0:
		return errors.New("child stdin pipe contains bytes beyond its declared length")
	case errors.Is(readErr, io.EOF):
		return nil
	case readErr != nil:
		return fmt.Errorf("verify child stdin transport terminator: %w", readErr)
	default:
		return errors.New("child stdin transport did not terminate with EOF")
	}
}

func awaitInputDelivery(source io.Reader, delivery <-chan error) error {
	select {
	case err := <-delivery:
		return err
	default:
	}
	if closer, ok := source.(io.Closer); ok {
		_ = closer.Close()
	}
	timer := time.NewTimer(maximumInputAbortWait)
	defer timer.Stop()
	select {
	case err := <-delivery:
		return err
	case <-timer.C:
		return errors.New("child stdin delivery did not stop after its source was revoked")
	}
}

func canonicalEnvironment(
	environment []ownerprotocol.EnvironmentEntry,
	eventFD int,
	identity ownerprotocol.Identity,
) []string {
	values := make(map[string]string, len(environment)+4)
	for _, entry := range environment {
		values[entry.Name] = entry.Value
	}
	if eventFD != 0 {
		values[testtrace.EventFDEnvironment] = fmt.Sprint(eventFD)
	}
	values[testrun.RunIDEnvironment] = identity.RunID
	values[testrun.OperationIDEnvironment] = identity.OperationID
	values[testrun.ScenarioEnvironment] = identity.Scenario
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+values[name])
	}
	return result
}
