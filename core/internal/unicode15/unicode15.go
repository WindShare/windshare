//go:generate node ../../../scripts/unicode15/generate-go.mjs

// Package unicode15 provides the frozen Unicode primitives used by versioned
// protocol identities. Runtime Unicode tables must never decide wire-visible
// equality because those tables advance independently on each peer.
package unicode15

import (
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	Version = generatedUnicodeVersion

	asciiLimit          = 0x80
	scalarLimit         = rune(0x11_0000)
	hangulSyllableBase  = rune(0xac00)
	hangulLeadingBase   = rune(0x1100)
	hangulVowelBase     = rune(0x1161)
	hangulTrailingBase  = rune(0x11a7)
	hangulLeadingCount  = 19
	hangulVowelCount    = 21
	hangulTrailingCount = 28
	hangulBlockCount    = hangulVowelCount * hangulTrailingCount
	hangulSyllableCount = hangulLeadingCount * hangulBlockCount
)

func isASCII(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] >= asciiLimit {
			return false
		}
	}
	return true
}

type normalizationTables struct {
	decompositions map[rune][]rune
	combiningClass map[rune]uint8
	compositions   map[uint64]rune
}

var (
	normalizationOnce   sync.Once
	pinnedNormalization normalizationTables
	caseFoldOnce        sync.Once
	pinnedCaseFold      map[rune]string
)

func normalization() *normalizationTables {
	normalizationOnce.Do(func() {
		pinnedNormalization = normalizationTables{
			decompositions: parseRuneMappings(canonicalDecompositionData),
			combiningClass: parseCombiningClasses(combiningClassData),
			compositions:   parseCompositions(canonicalCompositionData),
		}
	})
	return &pinnedNormalization
}

func caseFoldMappings() map[rune]string {
	caseFoldOnce.Do(func() {
		parsed := parseRuneMappings(caseFoldData)
		pinnedCaseFold = make(map[rune]string, len(parsed))
		for source, replacement := range parsed {
			pinnedCaseFold[source] = string(replacement)
		}
	})
	return pinnedCaseFold
}

func parseRuneMappings(data string) map[rune][]rune {
	mappings := make(map[rune][]rune)
	for record := range strings.FieldsSeq(data) {
		sourceText, replacementText, ok := strings.Cut(record, "=")
		if !ok {
			panic("unicode15: malformed rune mapping")
		}
		source := parseScalar(sourceText)
		parts := strings.Split(replacementText, ".")
		replacement := make([]rune, len(parts))
		for index, part := range parts {
			replacement[index] = parseScalar(part)
		}
		mappings[source] = replacement
	}
	return mappings
}

func parseCombiningClasses(data string) map[rune]uint8 {
	classes := make(map[rune]uint8)
	for record := range strings.FieldsSeq(data) {
		scalarText, classText, ok := strings.Cut(record, "=")
		if !ok {
			panic("unicode15: malformed combining-class record")
		}
		class, err := strconv.ParseUint(classText, 10, 8)
		if err != nil {
			panic("unicode15: malformed combining class")
		}
		classes[parseScalar(scalarText)] = uint8(class)
	}
	return classes
}

func parseCompositions(data string) map[uint64]rune {
	compositions := make(map[uint64]rune)
	for record := range strings.FieldsSeq(data) {
		pairText, compositeText, ok := strings.Cut(record, "=")
		if !ok {
			panic("unicode15: malformed composition record")
		}
		pair := strings.Split(pairText, ".")
		if len(pair) != 2 {
			panic("unicode15: malformed composition pair")
		}
		compositions[compositionKey(parseScalar(pair[0]), parseScalar(pair[1]))] = parseScalar(compositeText)
	}
	return compositions
}

func parseScalar(value string) rune {
	scalar, err := strconv.ParseUint(value, 16, 21)
	if err != nil || scalar >= uint64(scalarLimit) {
		panic("unicode15: malformed scalar")
	}
	return rune(scalar)
}

func compositionKey(starter, next rune) uint64 {
	return uint64(starter)*uint64(scalarLimit) + uint64(next)
}

func combiningClass(scalar rune) uint8 {
	return normalization().combiningClass[scalar]
}

func decomposeHangul(scalar rune) ([3]rune, int) {
	syllable := int(scalar - hangulSyllableBase)
	if syllable < 0 || syllable >= hangulSyllableCount {
		return [3]rune{}, 0
	}
	leading := hangulLeadingBase + rune(syllable/hangulBlockCount)
	vowel := hangulVowelBase + rune((syllable%hangulBlockCount)/hangulTrailingCount)
	trailing := syllable % hangulTrailingCount
	if trailing == 0 {
		return [3]rune{leading, vowel}, 2
	}
	return [3]rune{leading, vowel, hangulTrailingBase + rune(trailing)}, 3
}

func compose(starter, next rune) (rune, bool) {
	if composite, ok := composeHangul(starter, next); ok {
		return composite, true
	}
	composite, ok := normalization().compositions[compositionKey(starter, next)]
	return composite, ok
}

func composeHangul(starter, next rune) (rune, bool) {
	leading := int(starter - hangulLeadingBase)
	vowel := int(next - hangulVowelBase)
	if leading >= 0 && leading < hangulLeadingCount && vowel >= 0 && vowel < hangulVowelCount {
		return hangulSyllableBase + rune((leading*hangulVowelCount+vowel)*hangulTrailingCount), true
	}

	syllable := int(starter - hangulSyllableBase)
	trailing := int(next - hangulTrailingBase)
	if syllable >= 0 && syllable < hangulSyllableCount && syllable%hangulTrailingCount == 0 &&
		trailing > 0 && trailing < hangulTrailingCount {
		return starter + rune(trailing), true
	}
	return 0, false
}

func composeSegment(segment []rune) []rune {
	if len(segment) <= 1 {
		return segment
	}
	sort.SliceStable(segment, func(left, right int) bool {
		return combiningClass(segment[left]) < combiningClass(segment[right])
	})
	result := make([]rune, 0, len(segment))
	starterPosition := -1
	var starter rune
	var lastClass uint8
	for _, scalar := range segment {
		scalarClass := combiningClass(scalar)
		if starterPosition >= 0 && (lastClass < scalarClass || lastClass == 0) {
			if composite, composed := compose(starter, scalar); composed {
				result[starterPosition] = composite
				starter = composite
				continue
			}
		}
		result = append(result, scalar)
		if scalarClass == 0 {
			starterPosition = len(result) - 1
			starter = scalar
		}
		lastClass = scalarClass
	}
	return result
}

// NormalizeNFC applies canonical composition with the frozen Unicode 15.0.0
// tables, including algorithmic Hangul normalization.
func NormalizeNFC(value string) string {
	// ASCII scalars have combining class zero and neither decompose nor compose
	// with any adjacent ASCII scalar under Unicode 15.0.0.
	if isASCII(value) {
		return value
	}
	tables := normalization()
	var outputBuffer [64]rune
	var segmentBuffer [8]rune
	output := outputBuffer[:0]
	segment := segmentBuffer[:0]
	emit := func(scalars []rune) {
		output = append(output, scalars...)
	}
	accept := func(scalar rune) {
		scalarClass := tables.combiningClass[scalar]
		if scalarClass != 0 {
			segment = append(segment, scalar)
			return
		}
		if len(segment) == 0 {
			segment = append(segment, scalar)
			return
		}
		if len(segment) == 1 && tables.combiningClass[segment[0]] == 0 {
			if composite, ok := compose(segment[0], scalar); ok {
				segment[0] = composite
			} else {
				emit(segment)
				segment[0] = scalar
			}
			return
		}
		normalized := composeSegment(segment)
		if len(normalized) == 1 && tables.combiningClass[normalized[0]] == 0 {
			if composite, ok := compose(normalized[0], scalar); ok {
				segment = append(segment[:0], composite)
				return
			}
		}
		emit(normalized)
		segment = append(segment[:0], scalar)
	}

	for _, scalar := range value {
		if hangul, length := decomposeHangul(scalar); length > 0 {
			for index := range length {
				accept(hangul[index])
			}
			continue
		}
		if mapped := tables.decompositions[scalar]; mapped != nil {
			for _, decomposed := range mapped {
				accept(decomposed)
			}
			continue
		}
		accept(scalar)
	}
	emit(composeSegment(segment))
	return string(output)
}

// FullCaseFold applies Unicode 15.0.0's locale-independent default full case
// folding. Turkic tailoring is intentionally outside the portable identity.
func FullCaseFold(value string) string {
	// Within ASCII, Unicode 15.0.0 full case folding maps only A..Z to a..z.
	if isASCII(value) {
		firstUpper := -1
		for index := 0; index < len(value); index++ {
			if value[index] >= 'A' && value[index] <= 'Z' {
				firstUpper = index
				break
			}
		}
		if firstUpper < 0 {
			return value
		}
		folded := make([]byte, len(value))
		copy(folded[:firstUpper], value[:firstUpper])
		for index := firstUpper; index < len(value); index++ {
			octet := value[index]
			if octet >= 'A' && octet <= 'Z' {
				octet += 'a' - 'A'
			}
			folded[index] = octet
		}
		return string(folded)
	}
	mappings := caseFoldMappings()
	var output strings.Builder
	output.Grow(len(value))
	for _, scalar := range value {
		if replacement, ok := mappings[scalar]; ok {
			output.WriteString(replacement)
		} else {
			output.WriteRune(scalar)
		}
	}
	return output.String()
}

// FoldPortableName returns the stable collision identity defined by the path
// policy: full default case fold followed by canonical composition.
func FoldPortableName(value string) string {
	return NormalizeNFC(FullCaseFold(value))
}
