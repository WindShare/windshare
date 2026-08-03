package wire

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/windshare/windshare/internal/perfevidence"
)

const (
	MaximumProtocolLine  = 1 << 20
	MaximumCapturedBytes = 32 << 20
	TargetStartedEvent   = "target-started"
	TargetFinishedEvent  = "target-finished"
	TargetSettledEvent   = "target-settled"
)

type Initialization struct {
	RuntimeRoot       string     `json:"runtimeRoot"`
	PrivateRoot       string     `json:"privateRoot,omitempty"`
	BootstrapManifest string     `json:"bootstrapManifest,omitempty"`
	Roots             []RootSpec `json:"roots"`
}

type RootSpec struct {
	Name             string `json:"name"`
	HostPath         string `json:"hostPath"`
	SHA256           string `json:"sha256"`
	SourceDescriptor int    `json:"sourceDescriptor,omitempty"`
}

type Request struct {
	Shutdown              bool                               `json:"shutdown,omitempty"`
	ProcessIDAcknowledged bool                               `json:"processIdAcknowledged,omitempty"`
	Command               perfevidence.MutationDomainCommand `json:"command"`
}

type Frame struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type Response struct {
	Event              string    `json:"event,omitempty"`
	Error              string    `json:"error,omitempty"`
	Fatal              bool      `json:"fatal,omitempty"`
	NamespaceProcessID int       `json:"namespaceProcessId,omitempty"`
	ExitCode           int       `json:"exitCode"`
	StartedAt          time.Time `json:"startedAt,omitzero"`
	FinishedAt         time.Time `json:"finishedAt,omitzero"`
	Frames             []Frame   `json:"frames,omitempty"`
}

func WriteJSONLine(writer io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	_, err = io.Copy(writer, bytes.NewReader(encoded))
	return err
}

func ReadJSONLine(reader *bufio.Reader, destination any) error {
	line, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(line) > MaximumProtocolLine {
		return errors.New("private mutation protocol header exceeded its bound")
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(line, destination)
}

func ReadBoundedFrame(reader io.Reader, description Frame, maximum int64, destination io.Writer) ([]byte, error) {
	if description.Bytes < 0 || description.Bytes > maximum || len(description.SHA256) != 64 {
		return nil, errors.New("private mutation frame exceeded its declared bound")
	}
	hasher := sha256.New()
	var captured bytes.Buffer
	writers := []io.Writer{hasher}
	if destination == nil {
		writers = append(writers, &captured)
	} else {
		writers = append(writers, destination)
	}
	written, err := io.CopyN(io.MultiWriter(writers...), reader, description.Bytes)
	if err != nil || written != description.Bytes {
		return nil, errors.Join(fmt.Errorf("read %d of %d framed bytes", written, description.Bytes), err)
	}
	if observed := hex.EncodeToString(hasher.Sum(nil)); observed != description.SHA256 {
		return nil, fmt.Errorf("private mutation frame hash mismatch: got %s, want %s", observed, description.SHA256)
	}
	return captured.Bytes(), nil
}

func HashBytes(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}
