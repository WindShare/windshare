package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyCommittedBlobPreservesBinaryAndEmptyFrames(t *testing.T) {
	t.Parallel()

	const binaryID = "1111111111111111111111111111111111111111"
	const emptyID = "2222222222222222222222222222222222222222"
	payload := strings.Repeat("\x00\xff\r\n", 2048)
	replies := fmt.Sprintf("%s blob %d\n%s\n%s blob 0\n\n", binaryID, len(payload), payload, emptyID)
	responses := bufio.NewReader(strings.NewReader(replies))

	var destination bytes.Buffer
	if err := copyCommittedBlob(responses, binaryID, &destination); err != nil {
		t.Fatal(err)
	}
	if destination.String() != payload {
		t.Fatal("binary blob changed during staging")
	}
	destination.Reset()
	if err := copyCommittedBlob(responses, emptyID, &destination); err != nil {
		t.Fatal(err)
	}
	if destination.Len() != 0 {
		t.Fatal("empty blob received bytes from another frame")
	}
}

func TestStageCommittedBlobDiscardsInvalidFrame(t *testing.T) {
	t.Parallel()

	const blobID = "1111111111111111111111111111111111111111"
	const otherID = "2222222222222222222222222222222222222222"
	for _, test := range []struct {
		name  string
		reply string
	}{
		{name: "missing", reply: blobID + " missing\n"},
		{name: "wrong object", reply: otherID + " blob 0\n\n"},
		{name: "wrong type", reply: blobID + " tree 0\n\n"},
		{name: "negative size", reply: blobID + " blob -1\n\n"},
		{name: "short payload", reply: blobID + " blob 5\nabc"},
		{name: "invalid delimiter", reply: blobID + " blob 3\nabc!"},
	} {
		t.Run(test.name, func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "blob")
			responses := bufio.NewReader(strings.NewReader(test.reply))

			if err := stageCommittedBlob(responses, blobID, destination); err == nil {
				t.Fatal("staged an invalid blob frame")
			}
			if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid blob left a staged file: stat error %v", err)
			}
		})
	}
}
