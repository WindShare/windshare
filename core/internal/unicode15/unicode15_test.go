package unicode15

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
)

func TestGeneratedNormalizationData(t *testing.T) {
	digest := sha256.Sum256([]byte(canonicalDecompositionData + combiningClassData + canonicalCompositionData))
	if got := fmt.Sprintf("%X", digest); got != normalizationDataSHA256 {
		t.Fatalf("normalization data digest = %s, want %s", got, normalizationDataSHA256)
	}
	if Version != "15.0.0" || len(strings.Fields(canonicalDecompositionData)) != 2_061 ||
		len(strings.Fields(combiningClassData)) != 922 || len(strings.Fields(canonicalCompositionData)) != 941 {
		t.Fatalf("unexpected Unicode %s normalization data shape", Version)
	}
}

func TestGeneratedCaseFoldData(t *testing.T) {
	digest := sha256.Sum256([]byte(caseFoldData))
	if got, expected := fmt.Sprintf("%X", digest), "F05274D4172BCA0D663CBA2482E3EDAE17B7E608B12DEA8C16AB4CA75EF34831"; got != expected {
		t.Fatalf("case-fold data digest = %s, want %s", got, expected)
	}
	if caseFoldSourceSHA256 != "CDD49E55EAE3BBF1F0A3F6580C974A0263CB86A6A08DAA10FBF705B4808A56F7" ||
		len(strings.Fields(caseFoldData)) != 1_530 {
		t.Fatalf("unexpected Unicode %s case-fold data shape", Version)
	}
}

func TestNormalizeNFC(t *testing.T) {
	tests := map[string]string{
		"cafe\u0301":                "café",
		"q\u0315\u0300\u05ae\u0301": "q\u05ae\u0300\u0301\u0315",
		"\u0344":                    "\u0308\u0301",
		"\u1100\u1161\u11a8":        "\uac01",
		"\uac01":                    "\uac01",
		"\U000105d2\u0307":          "\U000105d2\u0307",
		"\U000105c9":                "\U000105c9",
	}
	for input, expected := range tests {
		if got := NormalizeNFC(input); got != expected {
			t.Errorf("NormalizeNFC(%q) = %q, want %q", input, got, expected)
		}
	}
}

func TestNormalizeNFCMatchesStableUnicode15Mappings(t *testing.T) {
	decompositions := parseRuneMappings(canonicalDecompositionData)
	for source, decomposition := range decompositions {
		for _, input := range []string{string(source), string(decomposition)} {
			if got, expected := NormalizeNFC(input), norm.NFC.String(input); got != expected {
				t.Fatalf("NormalizeNFC(%U) = %q, want stable result %q", []rune(input), got, expected)
			}
		}
	}
	for offset := range hangulSyllableCount {
		syllable := string(hangulSyllableBase + rune(offset))
		if got := NormalizeNFC(norm.NFD.String(syllable)); got != syllable {
			t.Fatalf("NormalizeNFC(Hangul %U) = %U", []rune(syllable), []rune(got))
		}
	}

	pool := append(recordSources(canonicalDecompositionData), recordSources(combiningClassData)...)
	pool = append(pool, 'A', hangulLeadingBase, hangulVowelBase, hangulTrailingBase+1)
	state := uint32(0x6d2b_79f5)
	for range 2_048 {
		value := make([]rune, 1+int(state%12))
		for index := range value {
			state = state*1_664_525 + 1_013_904_223
			value[index] = pool[int(state%uint32(len(pool)))]
		}
		input := string(value)
		if got, expected := NormalizeNFC(input), norm.NFC.String(input); got != expected {
			t.Fatalf("NormalizeNFC(%U) = %q, want stable result %q", value, got, expected)
		}
	}
}

func recordSources(data string) []rune {
	records := strings.Fields(data)
	sources := make([]rune, len(records))
	for index, record := range records {
		source, _, _ := strings.Cut(record, "=")
		sources[index] = parseScalar(strings.Split(source, ".")[0])
	}
	return sources
}

func TestFullCaseFold(t *testing.T) {
	for source, expected := range parseRuneMappings(caseFoldData) {
		if got := FullCaseFold(string(source)); got != string(expected) {
			t.Fatalf("FullCaseFold(%U) = %U, want %U", source, []rune(got), expected)
		}
	}

	tests := map[string]string{
		"Straße": "strasse",
		"ẞ-ﬃ-ſ":  "ss-ffi-s",
		"İ-I-ı":  "i\u0307-i-ı",
	}
	for input, expected := range tests {
		if got := FullCaseFold(input); got != expected {
			t.Errorf("FullCaseFold(%q) = %q, want %q", input, got, expected)
		}
	}
	if left, right := FoldPortableName("Ångström"), FoldPortableName("A\u030angstro\u0308m"); left != right {
		t.Fatalf("canonically equivalent folds differ: %q != %q", left, right)
	}
}
