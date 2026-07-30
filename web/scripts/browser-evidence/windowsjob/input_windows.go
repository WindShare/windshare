//go:build windows

package main

import (
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"io"
	"os"
)

func requireExactRawEOF(reader io.Reader) error {
	buffer := make([]byte, 1)
	count, err := reader.Read(buffer)
	buffer[0] = 0
	if count != 0 || !errors.Is(err, io.EOF) {
		return errors.New("raw stdin pipe contains undeclared bytes")
	}
	return nil
}

func readExactRawStdin(handleValue uintptr, authority *rawStdin) ([]byte, error) {
	if authority == nil {
		if handleValue != 0 {
			return nil, errors.New("launcher received an undeclared raw stdin handle")
		}
		return nil, nil
	}
	if handleValue == 0 {
		return nil, errors.New("launcher did not receive its declared raw stdin handle")
	}
	handle := windows.Handle(handleValue)
	if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return nil, fmt.Errorf("make launcher raw stdin handle private: %w", err)
	}
	reader := os.NewFile(handleValue, "windowsjob-raw-stdin")
	if reader == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("launcher raw stdin handle is invalid")
	}
	defer reader.Close()
	buffer := make([]byte, int(authority.ByteLength))
	success := false
	defer func() {
		if !success {
			for index := range buffer {
				buffer[index] = 0
			}
		}
	}()
	if _, err := io.ReadFull(reader, buffer); err != nil {
		return nil, fmt.Errorf("read declared raw stdin bytes: %w", err)
	}
	extra := make([]byte, 1)
	count, err := reader.Read(extra)
	extra[0] = 0
	if count != 0 || !errors.Is(err, io.EOF) {
		return nil, errors.New("raw stdin pipe exceeds its declared byte length")
	}
	success = true
	return buffer, nil
}
