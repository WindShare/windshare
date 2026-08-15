package directoryauthority

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
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

func TestPersistentOwnedDirectoryIDBindsTheEnrollmentClaim(t *testing.T) {
	directory := &preparingIdentityDirectory{
		fakeDirectory: &fakeDirectory{},
		prepared:      []byte("stable claim"),
		observed:      []byte("untrusted observation"),
	}
	first, err := PersistentOwnedDirectoryID(directory)
	if err != nil || first.IsZero() {
		t.Fatalf("first identity = (%x, %v)", first.Bytes(), err)
	}
	second, err := PersistentOwnedDirectoryID(directory)
	if err != nil || first != second {
		t.Fatalf("stable identity = (%x, %x, %v)", first.Bytes(), second.Bytes(), err)
	}
	other, err := PersistentOwnedDirectoryID(&preparingIdentityDirectory{
		fakeDirectory: &fakeDirectory{},
		prepared:      []byte("different claim"),
	})
	if err != nil || other.IsZero() || other == first {
		t.Fatalf("distinct identity = (%x, %v)", other.Bytes(), err)
	}
	if directory.prepareCalls != 2 || directory.observeCalls != 0 {
		t.Fatalf("identity calls = (prepare=%d observe=%d)", directory.prepareCalls, directory.observeCalls)
	}
}

func TestPersistentOwnedDirectoryIDRejectsUnprovableEnrollment(t *testing.T) {
	failure := errors.New("prepare identity")
	tests := []struct {
		name      string
		directory outputcap.Directory
		want      error
		cause     error
	}{
		{name: "nil directory", want: ErrRetainedAuthorityChanged},
		{name: "unsupported directory", directory: &fakeDirectory{}, want: outputcap.ErrRecoverableOutputUnsupported},
		{
			name: "preparation failure",
			directory: &preparingIdentityDirectory{
				fakeDirectory: &fakeDirectory{},
				prepareErr:    failure,
			},
			want: ErrRetainedAuthorityChanged, cause: failure,
		},
		{
			name:      "empty claim",
			directory: &preparingIdentityDirectory{fakeDirectory: &fakeDirectory{}},
			want:      ErrRetainedAuthorityChanged,
		},
		{
			name: "oversized claim",
			directory: &preparingIdentityDirectory{
				fakeDirectory: &fakeDirectory{},
				prepared:      make([]byte, outputcap.MaxRootIdentityClaimBytes+1),
			},
			want: ErrRetainedAuthorityChanged,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := PersistentOwnedDirectoryID(test.directory)
			if !errors.Is(err, test.want) || test.cause != nil && !errors.Is(err, test.cause) {
				t.Fatalf("identity error = %v", err)
			}
		})
	}
}

func TestOwnedDirectoryIDRejectsMissingAuthorityBeforeOpeningNativeState(t *testing.T) {
	claim := outputsession.DirectoryClaim{}
	for _, authority := range []*Authority{nil, {}} {
		if _, err := authority.OwnedDirectoryID(claim); !errors.Is(err, ErrInvalidClaim) {
			t.Fatalf("invalid claim error = %v", err)
		}
	}
}


