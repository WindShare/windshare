package outputprobe

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

const (
	NamePrefix          = ".windshare-output.probe-"
	ReservedPrefix      = ".windshare-output.probe"
	RandomBytes         = 16
	AllocationAttempts  = 16
	MaximumLeftovers    = 64
	StageName           = "stage"
	PublicStageName     = "live-stage"
	AnchorName          = "anchor"
	PublicationName     = "publication"
	RecordName          = "record"
	TemporaryRecordName = "record.tmp"
	CandidateName       = "candidate"
	InstalledName       = "installed"
)

var fileRemovalOrder = [...]string{
	PublicStageName, StageName, PublicationName, AnchorName, TemporaryRecordName, RecordName,
}
var directoryRemovalOrder = [...]string{CandidateName, InstalledName}

func NewName(random io.Reader) (string, error) {
	if random == nil {
		return "", unsafe("allocate probe", errors.New("random source is absent"))
	}
	var nonce [RandomBytes]byte
	if _, err := io.ReadFull(random, nonce[:]); err != nil {
		return "", err
	}
	return NamePrefix + hex.EncodeToString(nonce[:]), nil
}

func CanonicalName(name string) bool {
	if !strings.HasPrefix(name, NamePrefix) || len(name) != len(NamePrefix)+RandomBytes*2 {
		return false
	}
	for _, character := range name[len(NamePrefix):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

// Allocate owns only an exclusively created namespace. A collision never grants
// permission to inspect or remove the existing entry.
func Allocate(root outputcap.Directory, random io.Reader) (*Workspace, error) {
	for range AllocationAttempts {
		name, err := NewName(random)
		if err != nil {
			return nil, err
		}
		directory, err := root.CreateDirectory(name, true)
		if errors.Is(err, outputcap.ErrNamespaceCollision) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return NewWorkspace(root, name, directory), nil
	}
	return nil, unsafe("allocate probe", errors.New("exclusive namespace allocation exhausted"))
}

func unsafe(operation string, cause error) error {
	return errors.Join(outputcap.ErrUnsafeNamespace, fmt.Errorf("%s: %w", operation, cause))
}

func unsupported(operation string, cause error) error {
	return errors.Join(outputcap.ErrRecoverableOutputUnsupported, fmt.Errorf("%s: %w", operation, cause))
}
