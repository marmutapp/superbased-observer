package crossmount

import (
	"errors"
	"testing"
)

func TestIsWSL(t *testing.T) {
	t.Parallel()

	const binfmt = "/proc/sys/fs/binfmt_misc/WSLInterop"

	tests := []struct {
		name    string
		present map[string]bool
		files   map[string]string
		want    bool
	}{
		{
			name:    "binfmt interop present",
			present: map[string]bool{binfmt: true},
			want:    true,
		},
		{
			name:  "proc version names microsoft",
			files: map[string]string{"/proc/version": "Linux version 5.15.0-microsoft-standard-WSL2"},
			want:  true,
		},
		{
			name:  "proc version names Microsoft mixed case",
			files: map[string]string{"/proc/version": "Linux ... Microsoft ..."},
			want:  true,
		},
		{
			name:  "pure linux: neither signal",
			files: map[string]string{"/proc/version": "Linux version 6.1.0-generic"},
			want:  false,
		},
		{
			name: "no proc files at all",
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exists := func(p string) bool { return tc.present[p] }
			readFile := func(p string) ([]byte, error) {
				if v, ok := tc.files[p]; ok {
					return []byte(v), nil
				}
				return nil, errors.New("not found")
			}
			if got := isWSL(exists, readFile); got != tc.want {
				t.Errorf("isWSL = %v, want %v", got, tc.want)
			}
		})
	}
}
