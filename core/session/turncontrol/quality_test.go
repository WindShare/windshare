package turncontrol

import (
	"reflect"
	"testing"
)

func TestQualityRequiresExactLaneAndOrderedSamples(t *testing.T) {
	quality := Quality{Authority: Header{SessionID: [16]byte{1}, OwnerPathID: [16]byte{2}, Sequence: 1},
		PathID: [16]byte{3}, Lane: Lane{ID: 4, Epoch: 5}, NetworkGeneration: [16]byte{6}, SampleSequence: 7,
		ContentBytes: 1 << 40, BusyMilliseconds: 8000, PendingBytes: 1 << 20}
	body, err := Encode(quality)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(body)
	if err != nil || !reflect.DeepEqual(decoded, quality) {
		t.Fatalf("quality lost its measurement scope: %+v, %v", decoded, err)
	}
	for _, mutate := range []func(*Quality){func(q *Quality) { q.Lane = Lane{} }, func(q *Quality) { q.SampleSequence = 0 }, func(q *Quality) { q.PathID = [16]byte{} }} {
		invalid := quality
		mutate(&invalid)
		if _, err := Encode(invalid); err == nil {
			t.Fatal("accepted an unscoped measurement")
		}
	}
}
