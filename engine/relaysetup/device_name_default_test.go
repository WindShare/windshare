package relaysetup

import (
	"strings"
	"testing"
)

func TestDefaultDeviceNameProducesValidLabels(t *testing.T) {
	longestASCII := strings.Repeat("a", maximumDeviceNameBytes)
	for _, test := range []struct{ name, suggestion, want string }{
		{"ordinary", "Alice's laptop", "Alice's laptop"},
		{"unicode", "小明的电脑", "小明的电脑"},
		{"empty", "", fallbackDeviceName},
		{"blank and controls", " \t\n\r ", fallbackDeviceName},
		{"trim and controls", "\t Alice\n laptop\r ", "Alice laptop"},
		{"invalid UTF-8", "Alice\xff laptop", "Alice laptop"},
		{"only invalid UTF-8", "\xff", fallbackDeviceName},
		{"replacement rune", "\ufffd", "\ufffd"},
		{"byte limit", longestASCII, longestASCII},
		{"truncate ASCII", longestASCII + "a", longestASCII},
		{"truncate multibyte rune", longestASCII[:maximumDeviceNameBytes-1] + "界", longestASCII[:maximumDeviceNameBytes-1]},
		{"truncate emoji", longestASCII[:maximumDeviceNameBytes-2] + "😊", longestASCII[:maximumDeviceNameBytes-2]},
		{"trim truncated space", longestASCII[:maximumDeviceNameBytes-2] + " 界", longestASCII[:maximumDeviceNameBytes-2]},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := DefaultDeviceName(test.suggestion)
			if got != test.want {
				t.Fatalf("default = %q, want %q", got, test.want)
			}
			if err := ValidateDeviceName(got); err != nil {
				t.Fatalf("generated default is invalid: %v", err)
			}
			if DefaultDeviceName(got) != got {
				t.Fatal("generated default changes when reused")
			}
		})
	}
}
