package clievent

import (
	"errors"
	"net"
	"strings"
	"unicode/utf8"
)

var ErrInvalidFact = errors.New("CLI event fact is invalid")

type RelayScheme uint8

const (
	RelayWS RelayScheme = iota + 1
	RelayWSS
)

func (scheme RelayScheme) Name() (string, bool) {
	switch scheme {
	case RelayWS:
		return "ws", true
	case RelayWSS:
		return "wss", true
	default:
		return "", false
	}
}

type RelayAuthority struct {
	scheme RelayScheme
	host   string
	port   uint16
}

func NewRelayAuthority(scheme RelayScheme, host string, port uint16) (RelayAuthority, error) {
	if _, ok := scheme.Name(); !ok || host == "" || port == 0 || !utf8.ValidString(host) ||
		host != strings.ToLower(host) || strings.ContainsAny(host, "/?#@[]") {
		return RelayAuthority{}, ErrInvalidFact
	}
	for _, character := range host {
		if character <= 0x20 || character == 0x7f {
			return RelayAuthority{}, ErrInvalidFact
		}
	}
	// The domain normalizer permits ASCII DNS names and canonical IP literals.
	// This defensive check keeps arbitrary provider strings from becoming hosts
	// if a future caller bypasses commandprojection.
	if ip := net.ParseIP(host); ip == nil {
		for label := range strings.SplitSeq(strings.TrimSuffix(host, "."), ".") {
			if label == "" {
				return RelayAuthority{}, ErrInvalidFact
			}
			for _, character := range label {
				if character != '-' && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
					return RelayAuthority{}, ErrInvalidFact
				}
			}
		}
	}
	return RelayAuthority{scheme: scheme, host: host, port: port}, nil
}

func (authority RelayAuthority) Scheme() RelayScheme { return authority.scheme }
func (authority RelayAuthority) Host() string        { return authority.host }
func (authority RelayAuthority) Port() uint16        { return authority.port }
func (authority RelayAuthority) Valid() bool {
	_, err := NewRelayAuthority(authority.scheme, authority.host, authority.port)
	return err == nil
}

type SharingSubjectKind uint8

const (
	SharingFile SharingSubjectKind = iota + 1
	SharingDirectory
	SharingMultiple
)

func (kind SharingSubjectKind) Name() (string, bool) {
	switch kind {
	case SharingFile:
		return "file", true
	case SharingDirectory:
		return "directory", true
	case SharingMultiple:
		return "multiple", true
	default:
		return "", false
	}
}

type SharingSubject struct {
	kind          SharingSubjectKind
	name          DisplayName
	fileBytes     uint64
	selectedItems uint64
}

func NewFileSubject(name DisplayName, size uint64) (SharingSubject, error) {
	if name.Empty() {
		return SharingSubject{}, ErrInvalidFact
	}
	return SharingSubject{kind: SharingFile, name: name, fileBytes: size, selectedItems: 1}, nil
}

func NewDirectorySubject(name DisplayName) (SharingSubject, error) {
	if name.Empty() {
		return SharingSubject{}, ErrInvalidFact
	}
	return SharingSubject{kind: SharingDirectory, name: name, selectedItems: 1}, nil
}

func NewMultipleSubject(selectedItems uint64) (SharingSubject, error) {
	if selectedItems < 2 {
		return SharingSubject{}, ErrInvalidFact
	}
	return SharingSubject{kind: SharingMultiple, selectedItems: selectedItems}, nil
}

func (subject SharingSubject) Kind() SharingSubjectKind { return subject.kind }
func (subject SharingSubject) Name() DisplayName        { return subject.name }
func (subject SharingSubject) FileBytes() uint64        { return subject.fileBytes }
func (subject SharingSubject) SelectedItems() uint64    { return subject.selectedItems }
func (subject SharingSubject) Valid() bool {
	switch subject.kind {
	case SharingFile:
		return !subject.name.Empty() && subject.selectedItems == 1
	case SharingDirectory:
		return !subject.name.Empty() && subject.fileBytes == 0 && subject.selectedItems == 1
	case SharingMultiple:
		return subject.name.Empty() && subject.fileBytes == 0 && subject.selectedItems >= 2
	default:
		return false
	}
}
