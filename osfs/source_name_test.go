package osfs

import (
	"bytes"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestNativeReferencesPreserveNamesIndependentlyOfCatalogAdmission(t *testing.T) {
	names := []string{
		strings.Repeat("\u1112\u1161\u11ab", 40) + ".txt",
		strings.Repeat("\u0958", 85), strings.Repeat("\u754c", 100) + ".txt",
		"\u06a9\u062a\u0627\u0628\u200c\u0647\u0627.docx", "\U0001f468\u200d\U0001f469\u200d\U0001f467 photos", "soft\u00adhyphen",
		"\u200eIMG-0001.jpg", "tab\tname", "latin1-caf\xe9.txt",
	}
	if runtime.GOOS != "windows" {
		names = append(names, "12:30.log", "back\\slash")
	}
	for _, name := range names {
		full, err := NewSourceReference(0, "dir/"+name)
		if err != nil {
			t.Fatalf("native name %q rejected: %v", name, err)
		}
		location, err := parseSourceReference(full)
		if err != nil || location.RelativePath() != "dir/"+name {
			t.Fatalf("native spelling changed: %+v %v", location, err)
		}
		child, err := newChildSourceReference(sourceLocation{path: "dir"}, name)
		if err != nil || !bytes.Equal(child.Bytes(), full.Bytes()) {
			t.Fatalf("child locator differs from full locator: %q %v", name, err)
		}
	}
	for _, name := range []string{"\u200eIMG-0001.jpg", "tab\tname", "latin1-caf\xe9.txt"} {
		if _, err := catalog.CanonicalName(name); !errors.Is(err, catalog.ErrInvalidName) {
			t.Fatalf("private locator admission widened the public grammar for %q", name)
		}
	}
}

func TestChildSourceReferenceKeepsNativePathBounds(t *testing.T) {
	deep := sourceLocation{path: "a" + strings.Repeat("/a", catalog.MaxPathDepth-1)}
	if _, err := newChildSourceReference(deep, "b"); !errors.Is(err, catalog.ErrPathTooDeep) {
		t.Fatalf("too deep child = %v", err)
	}
	long := sourceLocation{path: strings.Repeat("a", maxNativeSourcePathBytes-2)}
	if _, err := newChildSourceReference(long, "b"); err != nil {
		t.Fatalf("child at native reference byte limit = %v", err)
	}
	if _, err := newChildSourceReference(long, "bb"); !errors.Is(err, catalog.ErrInvalidPath) {
		t.Fatalf("too long native child = %v", err)
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
	})
}
