package protocolsession

import (
	"bytes"
	"errors"
	"math"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/windshare/windshare/core/catalog"
)

const (
	SessionTerminalCodeFirst       = uint16(0x1001)
	SessionTerminalCodeLast        = uint16(0x1008)
	MaxSessionTerminalMessageBytes = 512
	controlSemanticSchemaVersion   = uint64(1)
)

var (
	ErrInvalidScanProgress    = errors.New("scan progress body is invalid")
	ErrInvalidSessionTerminal = errors.New("session terminal body is invalid")
)

type ScanProgress struct {
	AttemptID         catalog.ScanAttemptID
	DiscoveredEntries uint64
}

func EncodeScanProgress(progress ScanProgress) ([]byte, error) {
	if progress.AttemptID.IsZero() {
		return nil, ErrInvalidScanProgress
	}
	return EncodeBody(map[uint64]any{
		0: controlSemanticSchemaVersion,
		1: progress.AttemptID.Bytes(),
		2: progress.DiscoveredEntries,
	})
}

func DecodeScanProgress(encoded []byte) (ScanProgress, error) {
	if err := validateCanonicalBody(encoded); err != nil {
		return ScanProgress{}, errors.Join(ErrInvalidScanProgress, err)
	}
	var fields map[uint64]any
	if err := messageDecMode.Unmarshal(encoded, &fields); err != nil || len(fields) != 3 {
		return ScanProgress{}, ErrInvalidScanProgress
	}
	version, versionOK := fields[0].(uint64)
	attemptBytes, attemptOK := fields[1].([]byte)
	discovered, discoveredOK := fields[2].(uint64)
	if !versionOK || version != controlSemanticSchemaVersion || !attemptOK || !discoveredOK {
		return ScanProgress{}, ErrInvalidScanProgress
	}
	attempt, err := catalog.ScanAttemptIDFromBytes(attemptBytes)
	if err != nil || attempt.IsZero() {
		return ScanProgress{}, ErrInvalidScanProgress
	}
	progress := ScanProgress{AttemptID: attempt, DiscoveredEntries: discovered}
	canonical, err := EncodeScanProgress(progress)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return ScanProgress{}, errors.Join(ErrInvalidScanProgress, err)
	}
	return progress, nil
}

type SessionTerminal struct {
	Code    uint16
	Message string
}

func EncodeSessionTerminal(terminal SessionTerminal) ([]byte, error) {
	if terminal.Code < SessionTerminalCodeFirst || terminal.Code > SessionTerminalCodeLast ||
		terminal.Message == "" || !utf8.ValidString(terminal.Message) ||
		!norm.NFC.IsNormalString(terminal.Message) ||
		len(terminal.Message) > MaxSessionTerminalMessageBytes {
		return nil, ErrInvalidSessionTerminal
	}
	return EncodeBody(map[uint64]any{
		0: controlSemanticSchemaVersion,
		1: uint64(terminal.Code),
		2: terminal.Message,
	})
}

func DecodeSessionTerminal(encoded []byte) (SessionTerminal, error) {
	if err := validateCanonicalBody(encoded); err != nil {
		return SessionTerminal{}, errors.Join(ErrInvalidSessionTerminal, err)
	}
	var fields map[uint64]any
	if err := messageDecMode.Unmarshal(encoded, &fields); err != nil || len(fields) != 3 {
		return SessionTerminal{}, ErrInvalidSessionTerminal
	}
	version, versionOK := fields[0].(uint64)
	code, codeOK := fields[1].(uint64)
	message, messageOK := fields[2].(string)
	if !versionOK || version != controlSemanticSchemaVersion || !codeOK ||
		code > math.MaxUint16 || !messageOK {
		return SessionTerminal{}, ErrInvalidSessionTerminal
	}
	terminal := SessionTerminal{Code: uint16(code), Message: message}
	canonical, err := EncodeSessionTerminal(terminal)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return SessionTerminal{}, errors.Join(ErrInvalidSessionTerminal, err)
	}
	return terminal, nil
}
