package directoryauthority

import (
	"bytes"
	"errors"
	"testing"
)

type preparingIdentityDirectory struct {
	*fakeDirectory
	prepared     []byte
	observed     []byte
	prepareErr   error
	prepareCalls int
	observeCalls int
}

func (directory *preparingIdentityDirectory) PreparePersistentDirectoryIdentityClaim() ([]byte, error) {
	directory.prepareCalls++
	return append([]byte(nil), directory.prepared...), directory.prepareErr
}

func (directory *preparingIdentityDirectory) PersistentDirectoryIdentityClaim() ([]byte, error) {
	directory.observeCalls++
	return append([]byte(nil), directory.observed...), nil
}

type observingIdentityDirectory struct {
	*fakeDirectory
	claim []byte
	calls int
}

func (directory *observingIdentityDirectory) PersistentDirectoryIdentityClaim() ([]byte, error) {
	directory.calls++
	return append([]byte(nil), directory.claim...), nil
}

func TestPersistentDirectoryIdentityClaimKeepsEnrollmentExplicit(t *testing.T) {
	t.Run("preparer owns enrollment", func(t *testing.T) {
		failure := errors.New("prepare identity")
		directory := &preparingIdentityDirectory{
			fakeDirectory: &fakeDirectory{}, prepared: []byte("prepared"),
			observed: []byte("observed"), prepareErr: failure,
		}
		claim, supported, err := persistentDirectoryIdentityClaim(directory)
		if !supported || !bytes.Equal(claim, []byte("prepared")) || !errors.Is(err, failure) {
			t.Fatalf("claim=%q supported=%t err=%v", claim, supported, err)
		}
		if directory.prepareCalls != 1 || directory.observeCalls != 0 {
			t.Fatalf("prepare calls=%d observe calls=%d", directory.prepareCalls, directory.observeCalls)
		}
	})

	t.Run("read-only identity remains supported", func(t *testing.T) {
		directory := &observingIdentityDirectory{fakeDirectory: &fakeDirectory{}, claim: []byte("stable")}
		claim, supported, err := persistentDirectoryIdentityClaim(directory)
		if !supported || err != nil || !bytes.Equal(claim, directory.claim) || directory.calls != 1 {
			t.Fatalf("claim=%q supported=%t calls=%d err=%v", claim, supported, directory.calls, err)
		}
	})

	t.Run("missing identity is unsupported", func(t *testing.T) {
		claim, supported, err := persistentDirectoryIdentityClaim(&fakeDirectory{})
		if supported || err != nil || claim != nil {
			t.Fatalf("claim=%q supported=%t err=%v", claim, supported, err)
		}
	})
}
