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
		// WHATWG drops the line break, so the second link becomes part of the
		// first fragment and the key after its '#' is read. The first host wins.
		{"two links joined by a line break", full + "\nhttps://other.example/" + testShareID + "#" + testKey},
		{"two links joined by CRLF", full + "\r\nhttps://other.example/" + testShareID + "?r=https://r.example#" + testKey},
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

// Known differences: net/url rejects invalid percent escapes in any path
// segment and a NUL byte, while WHATWG keeps the former and percent-encodes the
// latter. The browser accepts these links when the bad segment is not the route.
// Pin the CLI's rejection so a change in either direction is deliberate.
func TestParseKnownBrowserDifferencesStayRejected(t *testing.T) {
	for _, raw := range []string{
		"https://ex.com/" + testShareID + "/%ZZ/..#" + testKey,
		"https://ex.com/%zz/" + testShareID + "#" + testKey,
		"https://ex.com/a\x00b/" + testShareID + "#" + testKey,
	} {
		if got, err := link.Parse(raw); err == nil {
			t.Fatalf("Parse(%s) = %+v; update the documented browser differences", strconv.Quote(raw), got)
		}
	}
}

// A separately entered key goes through the browser's capabilityFromInput: text
// containing "://" is parsed as a complete link (raw fragment, route checked
// against the key); anything else is a key payload, optionally after '#'.
func TestMergeResolvesSeparateKeyLikeBrowserKeyBox(t *testing.T) {
	base := "https://ex.com/" + testShareID
	want := testCapability
	want.Relays = []string{"https://ex.com"}
	for _, key := range []string{
		testKey,
		"# " + testKey,
		"\ufeff" + testKey + "\u3000",
		base + "#" + testKey,
		"https://other.example/app/" + testShareID + "#" + testKey,
	} {
		got, err := link.Merge(base, key)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Merge(base, %s) = %+v, %v; browser accepts it", strconv.Quote(key), got, err)
		}
	}
	for _, key := range []string{
		base + "# " + testKey,
		base + "#%" + strconv.FormatInt(int64(testKey[0]), 16) + testKey[1:],
		"https://ex.com/AAAAAAAAAAAAAAAA#" + testKey,
		"x://y#" + testKey,
		"\u0085" + testKey,
		testKey + "\u0085",
	} {
		if got, err := link.Merge(base, key); err == nil {
			t.Fatalf("Merge(base, %s) = %+v; browser rejects it", strconv.Quote(key), got)
		}
	}
	// A link without a key is a malformed key, not a request for another key.
	if _, err := link.Merge(base, base); !errors.Is(err, link.ErrMalformedFragment) || errors.Is(err, link.ErrMissingFragment) {
		t.Fatalf("Merge(base, base) = %v, want ErrMalformedFragment", err)
	}
}

func TestTrimCopiedTextUsesJavaScriptTrim(t *testing.T) {
	for raw, want := range map[string]string{
		"\ufeff\u00a0\u2028 key \t\r\n": "key",
		"\u0085key\u0085":               "\u0085key\u0085",
		"\x00key":                       "\x00key",
	} {
		if got := link.TrimCopiedText(raw); got != want {
			t.Fatalf("TrimCopiedText(%s) = %s, want %s", strconv.Quote(raw), strconv.Quote(got), strconv.Quote(want))
		}
	}
}
