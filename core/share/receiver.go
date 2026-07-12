package share

import (
	"crypto/rand"
	"fmt"
	"slices"

	"github.com/windshare/windshare/core/chunk"
	"github.com/windshare/windshare/core/internal/keyderiv"
	"github.com/windshare/windshare/core/layout"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/manifest"
	"github.com/windshare/windshare/core/session"
)

// Receiver authenticates a manifest and owns its immutable content geometry. Selection
// and write state belong to TransferPlan so one selector interpretation governs demand,
// materialization, progress, finalization, and resume identity.
type Receiver struct {
	dst           FileSink
	m             *manifest.Manifest
	lay           *layout.Layout
	codec         *chunk.Codec
	maxBlockBytes int64
}

// NewReceiver validates the complete geometry before any plan can allocate a bitfield.
func NewReceiver(l link.Link, sealedManifest []byte, dst FileSink) (*Receiver, error) {
	if dst == nil {
		return nil, fmt.Errorf("%w:dst", ErrNilDependency)
	}
	if l.Suite != link.SuiteAESGCM {
		return nil, fmt.Errorf("%w(suite 0x%02x)", link.ErrUnknownSuite, l.Suite)
	}
	if len(l.ReadSecret) != link.ReadSecretBytes {
		return nil, fmt.Errorf("%w: readSecret must be %d bytes, got %d", ErrBadLink, link.ReadSecretBytes, len(l.ReadSecret))
	}
	m, err := manifest.Open(keyderiv.ManifestKey(l.ReadSecret), sealedManifest)
	if err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}

	entries := make([]layout.Entry, len(m.Entries))
	for i, entry := range m.Entries {
		entries[i] = layout.Entry{Path: entry.Path, Size: entry.Size, IsDir: entry.IsDir}
	}
	lay, err := layout.New(entries, m.ChunkSize)
	if err != nil {
		return nil, err
	}
	maxBlockBytes, err := chunk.MaxSealedSize(l.Suite, m.ChunkSize)
	if err != nil {
		return nil, err
	}
	// Receivers only Open; the required RNG is never consumed.
	codec, err := chunk.NewCodec(keyderiv.StreamKey(l.ReadSecret), m.ChunkSize, rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Receiver{dst: dst, m: m, lay: lay, codec: codec, maxBlockBytes: maxBlockBytes}, nil
}

// Entries returns the authenticated manifest order without exposing mutable storage.
func (r *Receiver) Entries() []manifest.Entry { return slices.Clone(r.m.Entries) }

func (r *Receiver) NumChunks() uint64 { return r.lay.NumChunks() }

// MaxBlockBytes supplies the receive-session reassembly bound.
func (r *Receiver) MaxBlockBytes() int64 {
	return r.maxBlockBytes
}

// Opener exposes authenticated block opening to the transport-neutral session.
func (r *Receiver) Opener() session.Opener { return r.codec }
