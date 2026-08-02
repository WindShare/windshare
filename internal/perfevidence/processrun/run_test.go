package processrun

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

func TestRequestFromSpecEncodesNilArgumentsAsCanonicalEmptyArray(t *testing.T) {
	identity, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(t.TempDir())
	request, err := requestFromSpec(Spec{
		Identity:         identity,
		Executable:       filepath.Join(root, "fixture"),
		Arguments:        nil,
		WorkingDirectory: root,
		Environment:      []protocol.EnvironmentEntry{{Name: "ALPHA", Value: "one"}},
		Deadline:         time.Second,
		TerminationGrace: time.Second,
		AuthorizeStart:   func(protocol.StartEvidence) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.Command.Arguments == nil || len(request.Command.Arguments) != 0 {
		t.Fatalf("canonical arguments = %#v", request.Command.Arguments)
	}
	encoded, err := protocol.EncodeCanonical(request)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"arguments":[]`)) || bytes.Contains(encoded, []byte(`"arguments":null`)) {
		t.Fatalf("canonical request encoded arguments as %s", encoded)
	}
}
