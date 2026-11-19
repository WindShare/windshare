package records

import (
	"bytes"
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
	encoder    Encoder
	decoder    Decoder
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
	return benchmarkBlock{record: record, descriptor: descriptor, decoder: Decoder{ShareInstance: share}}

}

func BenchmarkFileLocalBlock(b *testing.B) {
	for _, blockBytes := range fileLocalBlockSizes {
		b.Run(fmt.Sprintf("block_bytes=%07d", blockBytes), func(b *testing.B) {
			fixture := prepareBenchmarkBlock(b, blockBytes)
			b.ReportAllocs()
			b.SetBytes(int64(blockBytes))
			var last []byte
			b.ResetTimer()
			for b.Loop() {
				var err error
				last, err = fixture.encoder.EncodeBlock(fixture.record)
				if err != nil {
					b.Fatal(err)
				}
				opened, err := fixture.decoder.DecodeBlock(fixture.descriptor, 0, last)
				if err != nil {
					b.Fatal(err)
				}
				checkBenchmarkBlock(b, opened, fixture.descriptor, blockBytes)
			}
			b.ReportMetric(float64(len(last)), "record-bytes/op")
			b.ReportMetric(float64(len(last)-int(blockBytes)), "record-overhead-bytes/op")
			b.ReportMetric(1, "file-local-blocks/op")
		})
	}
}

func BenchmarkOpenBlock(b *testing.B) {
	for _, blockBytes := range fileLocalBlockSizes {
		b.Run(fmt.Sprintf("block_bytes=%07d", blockBytes), func(b *testing.B) {
			fixture := prepareBenchmarkBlock(b, blockBytes)
			encoded, err := fixture.encoder.EncodeBlock(fixture.record)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(blockBytes))
			for b.Loop() {
				opened, err := fixture.decoder.DecodeBlock(fixture.descriptor, 0, encoded)
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
