package outputprobe

import "testing"

func TestObservationAcceptsOnlyTheNativeProbeVocabulary(t *testing.T) {
	observation := &Observation{}
	for name, size := range map[string]uint64{
		"stage": 0, "anchor": 1, "publication": 1, "record": 0, "record.tmp": 1,
	} {
		if err := observation.ObserveFile(name, size); err != nil {
			t.Fatalf("observe probe file %q: %v", name, err)
		}
	}
	for _, name := range []string{"candidate", "installed"} {
		if err := observation.ObserveDirectory(name); err != nil {
			t.Fatalf("observe probe directory %q: %v", name, err)
		}
	}

	if err := (*Observation)(nil).ObserveFile("stage", 0); err == nil {
		t.Fatal("nil probe observation accepted a file")
	}
	if err := (*Observation)(nil).ObserveDirectory("candidate"); err == nil {
		t.Fatal("nil probe observation accepted a directory")
	}
	if err := observation.ObserveFile("foreign", 0); err == nil {
		t.Fatal("unknown probe file was accepted")
	}
	if err := observation.ObserveDirectory("foreign"); err == nil {
		t.Fatal("unknown probe directory was accepted")
	}
}
