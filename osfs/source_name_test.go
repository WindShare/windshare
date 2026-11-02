package osfs

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestValidateSourceNameIsTheSharedNameRule(t *testing.T) {
	// 40 NFD Hangul syllables (HIEUH, A, NIEUN) are 364 native bytes but 124 after
	// NFC. macOS and Windows can hold this name; Linux cannot, so it is tested here.
	nfdKorean := strings.Repeat("\u1112\u1161\u11ab", 40) + ".txt"
	if len(nfdKorean) != 364 {
		t.Fatalf("fixture is %d bytes", len(nfdKorean))
	}
	if canonical, err := catalog.CanonicalName(nfdKorean); err != nil || len(canonical) != 124 {
		t.Fatalf("NFC fixture = %d bytes, %v", len(canonical), err)
	}
	// U+0958 is a composition exclusion: 255 native bytes become 510 after NFC.
	expanding := strings.Repeat("\u0958", 85)
	accepted := []string{"report.txt", "Cafe\u0301.txt", "\u6587\u4ef6.txt", strings.Repeat("a", catalog.MaxNameBytes), "CON", "trail."}
	rejected := []string{
		nfdKorean, expanding, strings.Repeat("a", catalog.MaxNameBytes+1), "12:30.log", `back\slash`, "tab\tname",
		"\u200eIMG-0001.jpg", "\u06a9\u062a\u0627\u0628\u200c\u0647\u0627.docx", "a\u200db", "soft\u00adhyphen",
		"latin1-caf\xe9.txt", "", ".", "..",
	}
	for _, name := range accepted {
		if err := validateSourceName(name); err != nil {
			t.Errorf("validateSourceName(%q) = %v", name, err)
		}
	}
	for _, name := range rejected {
		if err := validateSourceName(name); !errors.Is(err, catalog.ErrInvalidName) {
			t.Errorf("validateSourceName(%q) = %v, want ErrInvalidName", name, err)
		}
	}
	for _, name := range append(accepted, rejected...) {
		if name == "" || name == "." || name == ".." {
			continue
		}
		nameErr := validateSourceName(name)
		if _, err := NewSourceReference(0, "dir/"+name); (err == nil) != (nameErr == nil) {
			t.Errorf("NewSourceReference(dir/%q) = %v, name rule = %v", name, err, nameErr)
		}
		_, childErr := newChildSourceReference(sourceLocation{path: "dir"}, name)
		if (childErr == nil) != (nameErr == nil) || errors.Is(childErr, errUnrepresentableSourceName) != (nameErr != nil) {
			t.Errorf("newChildSourceReference(%q) = %v, name rule = %v", name, childErr, nameErr)
		}
	}
}

func TestChildSourceReferenceKeepsPathLimitsSeparateFromNames(t *testing.T) {
	deep := sourceLocation{path: "a" + strings.Repeat("/a", catalog.MaxPathDepth-1)}
	if _, err := newChildSourceReference(deep, "b"); !errors.Is(err, catalog.ErrPathTooDeep) || errors.Is(err, errUnrepresentableSourceName) {
		t.Fatalf("too deep child = %v", err)
	}
	// 127 full components plus one of 254 bytes leave room for exactly "/b".
	long := sourceLocation{path: strings.Repeat(strings.Repeat("a", catalog.MaxNameBytes)+"/", 127) + strings.Repeat("a", 254)}
	if _, err := newChildSourceReference(long, "b"); err != nil {
		t.Fatalf("child at the path byte limit = %v", err)
	}
	if _, err := newChildSourceReference(long, "bb"); !errors.Is(err, catalog.ErrInvalidPath) || errors.Is(err, errUnrepresentableSourceName) {
		t.Fatalf("too long child = %v", err)
	}
}

func FuzzChildSourceReferenceMatchesFullPathValidation(f *testing.F) {
	for _, seed := range [][2]string{
		{"", "a"}, {"dir", "file.txt"}, {"Cafe\u0301", "e\u0301"}, {"a/b", "12:30.log"}, {"x", "\u200e.jpg"},
		{"x", strings.Repeat("\u1112\u1161\u11ab", 40)}, {"x", strings.Repeat("\u0958", 85)}, {"x", "caf\xe9"},
		{strings.Repeat("a/", catalog.MaxPathDepth-2) + "a", "b"}, {"x", strings.Repeat("a", 256)},
	} {
		f.Add(uint16(1), seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, slot uint16, parentPath, name string) {
		// Directory entries are never empty and never contain '/'.
		if name == "" || strings.Contains(name, "/") {
			return
		}
		parentReference, err := NewSourceReference(RootSlot(slot), parentPath)
		if err != nil {
			return
		}
		parent, err := parseSourceReference(parentReference)
		if err != nil {
			t.Fatalf("parse of a fresh reference failed: %v", err)
		}
		joined := name
		if parentPath != "" {
			joined = parentPath + "/" + name
		}
		want, wantErr := NewSourceReference(RootSlot(slot), joined)
		got, gotErr := newChildSourceReference(parent, name)
		if (wantErr == nil) != (gotErr == nil) || !bytes.Equal(want.Bytes(), got.Bytes()) {
			t.Fatalf("child=%x,%v full=%x,%v", got.Bytes(), gotErr, want.Bytes(), wantErr)
		}
		if errors.Is(gotErr, errUnrepresentableSourceName) != (validateSourceName(name) != nil) {
			t.Fatalf("name classification = %v", gotErr)
		}
	})
}
