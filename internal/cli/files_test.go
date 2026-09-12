package cli

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestParseLocationSeparators(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		slash     bool
		invalid   bool
	}{
		{"local-forward", "new/", true, false},
		{"local-native", "new" + string(filepath.Separator), true, false},
		{"local-backslash", `new\`, runtime.GOOS == "windows", false},
		{"windows-absolute", `C:\new\`, runtime.GOOS == "windows", false},
		{"remote-forward", "bd://new/", true, false},
		{"remote-backslash", `bd://new\`, false, true},
		{"empty", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loc, err := parseLocation(tc.raw)
			if (err != nil) != tc.invalid || (!tc.invalid && loc.slash != tc.slash) {
				t.Fatalf("parseLocation(%q) = %+v, %v", tc.raw, loc, err)
			}
		})
	}
}
