package resumestate

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestStateCodecFailureMappingsRejectWellFormedInvalidClaims(t *testing.T) {
	if _, err := EncodeControl(Control{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero control encode error = %v", err)
	}
	if _, err := DecodeControl(nil); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("empty control decode error = %v", err)
	}
	baseControl := storedControlFromTestControl(t)
	for _, test := range []struct {
		name   string
		mutate func(*storedControl)
	}{
		{name: "schema", mutate: func(stored *storedControl) { stored.Schema++ }},
		{name: "durability code", mutate: func(stored *storedControl) { stored.Durability = 0xff }},
		{name: "power-loss binding", mutate: func(stored *storedControl) {
			stored.Durability = storedDurabilityPowerLoss
		}},
	} {
		t.Run("control "+test.name, func(t *testing.T) {
			stored := baseControl
			test.mutate(&stored)
			raw, err := encodeEnvelope(controlMagic, stored, MaxControlStateBytes)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeControl(raw); !errors.Is(err, ErrCorruptState) {
				t.Fatalf("decode error = %v", err)
			}
		})
	}

	if _, err := EncodeHeader(Header{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero header encode error = %v", err)
	}
	storedHeader := storeHeader(testHeader(t))
	storedHeader.Lifecycle = 0xff
	invalidHeader, err := encodeEnvelope(headerMagic, storedHeader, MaxSessionHeaderBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeHeader(invalidHeader); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("invalid header binding error = %v", err)
	}

	if _, err := EncodeFileRecord(BoundFileRecord{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero file record encode error = %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*storedFileRecord)
	}{
		{name: "zero share", mutate: func(stored *storedFileRecord) {
			stored.ShareInstance = make([]byte, len(stored.ShareInstance))
		}},
		{name: "overlapping ranges", mutate: func(stored *storedFileRecord) {
			stored.DurableRanges = [][2]uint64{{0, 5}, {4, 6}}
		}},
		{name: "invalid modified time", mutate: func(stored *storedFileRecord) {
			stored.ModifiedTime = storedModifiedTime{
				Present: true, Nanoseconds: 1_000_000_000, Precision: storedTimePrecisionNanoseconds,
			}
		}},
	} {
		t.Run("file "+test.name, func(t *testing.T) {
			stored := storedFileRecordFromTestRecord(t)
			test.mutate(&stored)
			raw, err := encodeEnvelope(fileMagic, stored, MaxFileStateBytes)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeFileRecord(raw); !errors.Is(err, ErrCorruptState) {
				t.Fatalf("decode error = %v", err)
			}
		})
	}
}

func TestStateCodecHelpersPreserveErrorAndTimeTaxonomy(t *testing.T) {
	if _, err := encodeEnvelope(controlMagic, make(chan int), MaxControlStateBytes); err == nil {
		t.Fatal("unsupported CBOR value encoded")
	}
	if _, err := encodeEnvelope(controlMagic, storedControl{}, 1); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("bounded envelope error = %v", err)
	}

	seconds, err := catalog.NewModifiedTime(1, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if stored := storeModifiedTime(seconds); stored.Precision != storedTimePrecisionSeconds {
		t.Fatalf("seconds precision code = %d", stored.Precision)
	}
	nanoseconds, err := catalog.NewModifiedTime(1, 1, catalog.TimePrecisionNanoseconds)
	if err != nil {
		t.Fatal(err)
	}
	if stored := storeModifiedTime(nanoseconds); stored.Precision != storedTimePrecisionNanoseconds {
		t.Fatalf("nanoseconds precision code = %d", stored.Precision)
	}

	if _, err := restoreModifiedTime(storedModifiedTime{Seconds: 1}); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("valued absent modified time error = %v", err)
	}
	if restored, err := restoreModifiedTime(storedModifiedTime{}); err != nil || restored.Present() {
		t.Fatalf("absent modified time = (%+v, %v)", restored, err)
	}
	if _, err := restoreModifiedTime(storedModifiedTime{
		Present: true, Nanoseconds: 1_000_000_000, Precision: storedTimePrecisionNanoseconds,
	}); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("invalid present modified time error = %v", err)
	}
}

func storedControlFromTestControl(t *testing.T) storedControl {
	t.Helper()
	control := testControl(t)
	raw, err := EncodeControl(control)
	if err != nil {
		t.Fatal(err)
	}
	var stored storedControl
	if err := decodeEnvelope(raw, controlMagic, MaxControlStateBytes, &stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

func storedFileRecordFromTestRecord(t *testing.T) storedFileRecord {
	t.Helper()
	raw, err := EncodeFileRecord(testBoundFileRecord(t, FilePublished))
	if err != nil {
		t.Fatal(err)
	}
	var stored storedFileRecord
	if err := decodeEnvelope(raw, fileMagic, MaxFileStateBytes, &stored); err != nil {
		t.Fatal(err)
	}
	return stored
}
