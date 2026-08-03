package wire

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestJSONLineRoundTripAndBound(t *testing.T) {
	var encoded bytes.Buffer
	want := Response{Event: TargetFinishedEvent, ExitCode: 7}
	if err := WriteJSONLine(&encoded, want); err != nil {
		t.Fatalf("WriteJSONLine() error = %v", err)
	}
	var got Response
	if err := ReadJSONLine(bufio.NewReaderSize(&encoded, MaximumProtocolLine), &got); err != nil {
		t.Fatalf("ReadJSONLine() error = %v", err)
	}
	if got.Event != want.Event || got.ExitCode != want.ExitCode {
		t.Fatalf("response = %+v, want %+v", got, want)
	}

	overflow := bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", MaximumProtocolLine+1)+"\n"), MaximumProtocolLine)
	if err := ReadJSONLine(overflow, &got); err == nil || err.Error() != "private mutation protocol header exceeded its bound" {
		t.Fatalf("ReadJSONLine(over bound) error = %v", err)
	}
}

func TestReadBoundedFrameValidatesIdentity(t *testing.T) {
	content := []byte("sealed output")
	description := Frame{Bytes: int64(len(content)), SHA256: HashBytes(content)}
	got, err := ReadBoundedFrame(bytes.NewReader(content), description, int64(len(content)), nil)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("ReadBoundedFrame() = (%q, %v), want (%q, nil)", got, err, content)
	}

	description.SHA256 = strings.Repeat("0", 64)
	if _, err := ReadBoundedFrame(bytes.NewReader(content), description, int64(len(content)), nil); err == nil {
		t.Fatal("ReadBoundedFrame(hash mismatch) error = nil")
	}
	description.Bytes = int64(len(content) + 1)
	if _, err := ReadBoundedFrame(bytes.NewReader(content), description, int64(len(content)), nil); err == nil {
		t.Fatal("ReadBoundedFrame(over bound) error = nil")
	}

	short := Frame{Bytes: int64(len(content) + 1), SHA256: HashBytes(content)}
	if _, err := ReadBoundedFrame(bytes.NewReader(content), short, short.Bytes, nil); err == nil {
		t.Fatal("ReadBoundedFrame(short input) error = nil")
	}
}
