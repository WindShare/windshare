package records

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

var fileLocalBlockSizes = [...]uint32{
	catalog.MinChunkSize,
	64 << 10,
	catalog.DefaultChunkSize,
	catalog.MaxChunkSize,
}

type benchmarkNonceSource struct{ next uint64 }

func (source *benchmarkNonceSource) Read(destination []byte) (int, error) {
	clear(destination)
	source.next++
	if len(destination) >= 8 {
		binary.BigEndian.PutUint64(destination[len(destination)-8:], source.next)
	} else {
		for index := range destination {
			destination[index] = byte(source.next >> (8 * index))
		}
	}
	return len(destination), nil
}

func benchmarkRecordsIdentity[T ~[catalog.IdentityBytes]byte](seed byte) T {
	var identity T
	for index := range identity {
		identity[index] = seed + byte(index)
	}
	return identity
}

type benchmarkBlock struct {
	record     BlockRecord
	descriptor content.FileRevisionDescriptor
	sealer     *Sealer
	opener     *Opener
}

func prepareBenchmarkBlock(b *testing.B, blockBytes uint32) benchmarkBlock {
	b.Helper()
	share := benchmarkRecordsIdentity[catalog.ShareInstance](11)
	file := benchmarkRecordsIdentity[catalog.FileID](29)
	revision := benchmarkRecordsIdentity[content.FileRevision](47)
	geometry, err := content.NewFileGeometry(uint64(blockBytes), blockBytes)
	if err != nil {
		b.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(share, file, revision, geometry, catalog.ModifiedTime{})
	if err != nil {
		b.Fatal(err)
	}
	record, err := NewBlockRecord(descriptor, 0, bytes.Repeat([]byte{0xa5}, int(blockBytes)))
	if err != nil {
		b.Fatal(err)
	}
	readSecret := bytes.Repeat([]byte{0x5a}, content.ReadSecretBytes)
	keys, err := content.NewKeyTree(readSecret, share)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(keys.Destroy)
	seed := bytes.Repeat([]byte{0x3c}, ed25519.SeedSize)
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	sealer, err := NewSealer(SealerConfig{
		ShareInstance: share, Keys: keys, SigningKey: privateKey,
		NonceSource: &benchmarkNonceSource{}, MaxSealsPerKey: ^uint64(0),
	})
	if err != nil {
		b.Fatal(err)
	}
	opener, err := NewOpener(OpenerConfig{ShareInstance: share, Keys: keys, Sender: checkedSender(b, publicKey)})
	if err != nil {
		b.Fatal(err)
	}

	return benchmarkBlock{record: record, descriptor: descriptor, sealer: sealer, opener: opener}
}

func BenchmarkFileLocalBlock(b *testing.B) {
	for _, blockBytes := range fileLocalBlockSizes {
		b.Run(fmt.Sprintf("block_bytes=%07d", blockBytes), func(b *testing.B) {
			fixture := prepareBenchmarkBlock(b, blockBytes)
			b.ReportAllocs()
			b.SetBytes(int64(blockBytes))
			var last SealedBlock
			b.ResetTimer()
			for b.Loop() {
				var err error
				last, err = fixture.sealer.SealBlock(fixture.record)
				if err != nil {
					b.Fatal(err)
				}
				opened, err := fixture.opener.OpenBlock(fixture.descriptor, 0, last.Object)
				if err != nil {
					b.Fatal(err)
				}
				checkBenchmarkBlock(b, opened, fixture.descriptor, blockBytes)
			}
			b.ReportMetric(float64(len(last.Object)), "sealed-bytes/op")
			b.ReportMetric(float64(len(last.Object)-int(blockBytes)), "record-overhead-bytes/op")
			b.ReportMetric(1, "file-local-blocks/op")
		})
	}
}

func BenchmarkOpenBlock(b *testing.B) {
	for _, blockBytes := range fileLocalBlockSizes {
		b.Run(fmt.Sprintf("block_bytes=%07d", blockBytes), func(b *testing.B) {
			fixture := prepareBenchmarkBlock(b, blockBytes)
			sealed, err := fixture.sealer.SealBlock(fixture.record)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(blockBytes))
			for b.Loop() {
				opened, err := fixture.opener.OpenBlock(fixture.descriptor, 0, sealed.Object)
				if err != nil {
					b.Fatal(err)
				}
				checkBenchmarkBlock(b, opened, fixture.descriptor, blockBytes)
			}
		})
	}
}

func checkBenchmarkBlock(b *testing.B, opened BlockRecord, descriptor content.FileRevisionDescriptor, blockBytes uint32) {
	b.Helper()
	if opened.Descriptor() != descriptor || opened.LocalBlockIndex() != 0 || opened.DataLength() != int(blockBytes) {
		b.Fatal("file-local block identity changed during open")
	}
}
