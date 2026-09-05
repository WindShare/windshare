package main

import (
	"archive/zip"
	"errors"
	"io"
	"os"
	"path/filepath"
)

func copyFile(source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		_ = input.Close()
		return err
	}
	_, copyErr := io.Copy(output, input)
	return errors.Join(copyErr, input.Close(), output.Close())
}

func archiveDirectory(destination, directory, prefix, extension string) error {
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	writer := zip.NewWriter(output)
	walkErr := filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		header := &zip.FileHeader{Name: prefix + filepath.ToSlash(relative), Method: zip.Deflate}
		header.SetMode(0o644)
		if relative == "wind"+extension || relative == "wsrelay"+extension {
			header.SetMode(0o755)
		}
		entryWriter, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(entryWriter, input)
		return errors.Join(copyErr, input.Close())
	})
	return errors.Join(walkErr, writer.Close(), output.Close())
}
