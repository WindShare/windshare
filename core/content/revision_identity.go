package content

import (
	"encoding/binary"
	"errors"

	"github.com/windshare/windshare/core/catalog"
)

// RevisionEvidence binds normalized catalog evidence used to derive best-effort
// SourceMetadata comparison tokens across handle reopens.
type RevisionEvidence struct {
	shareInstance    catalog.ShareInstance
	fileID           catalog.FileID
	sourceIdentity   catalog.SourceIdentity
	versionCandidate catalog.VersionCandidate
	expectedSize     uint64
	modifiedTime     catalog.ModifiedTime
	chunkSize        uint32
}

func NewRevisionEvidence(
	shareInstance catalog.ShareInstance,
	fileID catalog.FileID,
	sourceIdentity catalog.SourceIdentity,
	versionCandidate catalog.VersionCandidate,
	expectedSize uint64,
	modifiedTime catalog.ModifiedTime,
	chunkSize uint32,
) (RevisionEvidence, error) {
	if shareInstance.IsZero() || fileID.IsZero() || sourceIdentity.IsZero() || versionCandidate.IsZero() {
		return RevisionEvidence{}, errors.New("revision evidence requires share, file, source, and version identities")
	}
	if expectedSize > catalog.MaxFileSize {
		return RevisionEvidence{}, errors.New("revision evidence size exceeds the portable file limit")
	}
	if _, err := NewFileGeometry(expectedSize, chunkSize); err != nil {
		return RevisionEvidence{}, err
	}
	if modifiedTime.Present() {
		normalized, err := catalog.NewModifiedTime(
			modifiedTime.Seconds(), modifiedTime.Nanoseconds(), modifiedTime.Precision(),
		)
		if err != nil || normalized != modifiedTime {
			return RevisionEvidence{}, errors.New("revision evidence contains a non-canonical modified time")
		}
	} else if modifiedTime.Seconds() != 0 || modifiedTime.Nanoseconds() != 0 || modifiedTime.Precision() != catalog.TimePrecisionUnknown {
		return RevisionEvidence{}, errors.New("revision evidence contains malformed absent modified time")
	}
	return RevisionEvidence{
		shareInstance: shareInstance, fileID: fileID, sourceIdentity: sourceIdentity,
		versionCandidate: versionCandidate, expectedSize: expectedSize,
		modifiedTime: modifiedTime, chunkSize: chunkSize,
	}, nil
}

func (e RevisionEvidence) ShareInstance() catalog.ShareInstance       { return e.shareInstance }
func (e RevisionEvidence) FileID() catalog.FileID                     { return e.fileID }
func (e RevisionEvidence) SourceIdentity() catalog.SourceIdentity     { return e.sourceIdentity }
func (e RevisionEvidence) VersionCandidate() catalog.VersionCandidate { return e.versionCandidate }
func (e RevisionEvidence) ExpectedSize() uint64                       { return e.expectedSize }
func (e RevisionEvidence) ModifiedTime() catalog.ModifiedTime         { return e.modifiedTime }
func (e RevisionEvidence) ChunkSize() uint32                          { return e.chunkSize }

func canonicalRevisionEvidence(evidence RevisionEvidence) []byte {
	encoded := make([]byte, 0, 16+16+len(evidence.sourceIdentity.Bytes())+len(evidence.versionCandidate.Bytes())+64)
	encoded = appendRevisionEvidenceField(encoded, evidence.shareInstance.Bytes())
	encoded = appendRevisionEvidenceField(encoded, evidence.fileID.Bytes())
	encoded = appendRevisionEvidenceField(encoded, evidence.sourceIdentity.Bytes())
	encoded = appendRevisionEvidenceField(encoded, evidence.versionCandidate.Bytes())
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], evidence.expectedSize)
	encoded = appendRevisionEvidenceField(encoded, size[:])
	modified := make([]byte, 1+8+4+1)
	if evidence.modifiedTime.Present() {
		modified[0] = 1
	}
	binary.BigEndian.PutUint64(modified[1:9], uint64(evidence.modifiedTime.Seconds()))
	binary.BigEndian.PutUint32(modified[9:13], evidence.modifiedTime.Nanoseconds())
	modified[13] = byte(evidence.modifiedTime.Precision())
	encoded = appendRevisionEvidenceField(encoded, modified)
	var chunk [4]byte
	binary.BigEndian.PutUint32(chunk[:], evidence.chunkSize)
	return appendRevisionEvidenceField(encoded, chunk[:])
}

func appendRevisionEvidenceField(destination, field []byte) []byte {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(field)))
	destination = append(destination, length[:]...)
	return append(destination, field...)
}
