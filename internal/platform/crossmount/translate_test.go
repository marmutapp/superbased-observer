package crossmount

import (
	"runtime"
	"testing"
)

func TestTranslateForeignPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("translation only fires on non-Windows hosts")
	}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"unix path", "/home/me/proj", "/home/me/proj"},
		{"relative", "src/main.go", "src/main.go"},
		{"windows backslash", `c:\Users\auzy_\proj`, "/mnt/c/Users/auzy_/proj"},
		{"windows backslash uppercase drive", `C:\programsx\regulation`, "/mnt/c/programsx/regulation"},
		{"windows forward slash", "c:/foo/bar", "/mnt/c/foo/bar"},
		{"windows mixed slash", `c:\foo/bar\baz`, "/mnt/c/foo/bar/baz"},
		{"windows D drive", `D:\work\thing`, "/mnt/d/work/thing"},
		{"unc path not translated", `\\server\share\path`, `\\server\share\path`},
		{"colon mid-path not translated", "a:b:c", "a:b:c"},
		{"too short", "c:", "c:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TranslateForeignPath(tc.in); got != tc.want {
				t.Errorf("TranslateForeignPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
