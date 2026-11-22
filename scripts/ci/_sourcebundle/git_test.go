package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestConsumeCommittedBlobPreservesBinaryAndEmptyFrames(t *testing.T) {
	t.Parallel()

	const binaryID = "1111111111111111111111111111111111111111"
	const emptyID = "2222222222222222222222222222222222222222"
	payload := strings.Repeat("\x00\xff\r\n", 2048)
	replies := fmt.Sprintf("%s blob %d\n%s\n%s blob 0\n\n", binaryID, len(payload), payload, emptyID)
	responses := bufio.NewReader(strings.NewReader(replies))

	var binaryBytes bytes.Buffer
	if err := consumeCommittedBlob(responses, committedFile{relativePath: "binary.dat", objectID: binaryID}, func(_ committedFile, size int64, body io.Reader) error {
		if size != int64(len(payload)) {
			return fmt.Errorf("size = %d, want %d", size, len(payload))
		}
		_, err := io.Copy(&binaryBytes, body)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if binaryBytes.String() != payload {
		t.Fatal("binary blob changed during streaming")
	}

	var emptyBytes bytes.Buffer
	if err := consumeCommittedBlob(responses, committedFile{relativePath: "empty.dat", objectID: emptyID}, func(_ committedFile, size int64, body io.Reader) error {
		if size != 0 {
			return fmt.Errorf("size = %d, want 0", size)
		}
		_, err := io.Copy(&emptyBytes, body)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if emptyBytes.Len() != 0 {
		t.Fatal("empty blob received bytes from another frame")
	}
}

func TestConsumeCommittedBlobRejectsInvalidFrames(t *testing.T) {
	t.Parallel()

	const blobID = "1111111111111111111111111111111111111111"
	const otherID = "2222222222222222222222222222222222222222"
	for _, test := range []struct {
		name    string
		reply   string
		consume blobConsumer
	}{
		{name: "missing", reply: blobID + " missing\n"},
		{name: "wrong object", reply: otherID + " blob 0\n\n"},
		{name: "wrong type", reply: blobID + " tree 0\n\n"},
		{name: "negative size", reply: blobID + " blob -1\n\n"},
		{name: "short payload", reply: blobID + " blob 5\nabc"},
		{name: "invalid delimiter", reply: blobID + " blob 3\nabc!"},
		{
			name:  "unread bytes",
			reply: blobID + " blob 3\nabc\n",
			consume: func(_ committedFile, _ int64, _ io.Reader) error {
				return nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			responses := bufio.NewReader(strings.NewReader(test.reply))
			consumer := test.consume
			if consumer == nil {
				consumer = func(_ committedFile, _ int64, body io.Reader) error {
					_, err := io.Copy(io.Discard, body)
					return err
				}
			}
			if err := consumeCommittedBlob(responses, committedFile{relativePath: "blob", objectID: blobID}, consumer); err == nil {
				t.Fatal("accepted an invalid blob frame")
			}
		})
	}
}
