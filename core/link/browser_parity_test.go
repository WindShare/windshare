package link_test

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/link"
)

// Browsers resolve a pasted capability with JavaScript trimming and WHATWG URL
// parsing. The CLI must accept and reject the same copied spellings, so a link
// that opens in one receiver cannot fail or change meaning in the other.
func TestParseResolvesCopiedSpellingsLikeBrowser(t *testing.T) {
	base := "https://ex.com/" + testShareID
	full := base + "#" + testKey
	accepted := []struct{ name, raw string }{
		{"byte order mark prefix", "\ufeff" + full},
		{"Unicode space boundaries", "\u00a0" + full + "\u3000"},
		{"C0 control boundaries", "\x00" + full + "\x1f"},
		{"newline inside route", "https://ex.com/" + testShareID[:5] + "\n" + testShareID[5:] + "#" + testKey},
		{"line break inside key", base + "#" + testKey[:20] + "\r\n" + testKey[20:]},
		{"tab inside host", "https://e\tx.com/" + testShareID + "#" + testKey},
		{"trailing single-dot segment", base + "/.#" + testKey},
		{"trailing encoded single-dot segment", base + "/%2E/#" + testKey},
		{"trailing double-dot segment", base + "/x/..#" + testKey},
		{"trailing encoded double-dot segment", base + "/x/.%2e#" + testKey},
		{"double-dot removes empty segment", base + "//..#" + testKey},
		{"trailing key space", full + " "},
		{"repeated fragment delimiter", base + "##" + testKey},
	}
	want := testCapability
	want.Relays = []string{"https://ex.com"}
	for _, test := range accepted {
		t.Run(test.name, func(t *testing.T) {
			got, err := link.Parse(test.raw)
			if err != nil {
				t.Fatalf("Parse(%s) = %v, browser accepts it", strconv.Quote(test.raw), err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Parse(%s) = %+v, want %+v", strconv.Quote(test.raw), got, want)
			}
		})
	}

	encodedFirst := "%" + strconv.FormatInt(int64(testKey[0]), 16) + testKey[1:]
	rejected := []struct{ name, raw string }{
		{"next-line prefix", "\u0085" + full},
		{"percent-encoded key character", base + "#" + encodedFirst},
		{"space-prefixed key", base + "# " + testKey},
		{"no-break-space-suffixed key", full + "\u00a0x"},
		{"encoded slash after route", base + "%2F#" + testKey},
		{"encoded slash before route", "https://ex.com/x%2F" + testShareID + "#" + testKey},
		{"double-dot removes route", base + "/..#" + testKey},
	}
	for _, test := range rejected {
		t.Run(test.name, func(t *testing.T) {
			if got, err := link.Parse(test.raw); err == nil || errors.Is(err, link.ErrMissingFragment) {
				t.Fatalf("Parse(%s) = %+v, %v; browser rejects it", strconv.Quote(test.raw), got, err)
			}
		})
	}
}

func TestMergeAndSplitUseBrowserFragmentSpelling(t *testing.T) {
	base := "https://ex.com/" + testShareID
	encodedFirst := "%" + strconv.FormatInt(int64(testKey[0]), 16) + testKey[1:]
	if _, err := link.Merge(base+"#"+encodedFirst, testKey); err == nil {
		t.Fatal("Merge accepted an embedded key the browser cannot decode")
	}
	if _, _, err := link.Split(base + "#" + encodedFirst); err == nil {
		t.Fatal("Split accepted a key the browser cannot decode")
	}
	bare, key, err := link.Split(" \ufeff" + base + "/.#" + testKey + "\n")
	if err != nil || key != testKey || strings.ContainsAny(bare, "#\n") {
		t.Fatalf("Split = %q, %q, %v", bare, key, err)
	}
	if _, err := link.Merge(bare, "\ufeff"+testKey); err != nil {
		t.Fatalf("Merge with byte order mark key: %v", err)
	}
}
