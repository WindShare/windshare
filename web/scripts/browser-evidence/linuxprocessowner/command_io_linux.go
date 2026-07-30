//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
)

func streamChildInput(
	source io.Reader,
	destination *os.File,
	authority *stdinAuthority,
) error {
	defer destination.Close()
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
	if count != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		return errors.New("child stdin pipe contains bytes beyond its declared length")
	}
	return nil
}

func canonicalEnvironment(environment map[string]string) []string {
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+environment[name])
	}
	return result
}
