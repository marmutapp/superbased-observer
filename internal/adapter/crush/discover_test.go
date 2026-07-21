package crush

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadProjectDataDirs(t *testing.T) {
	dir := t.TempDir()
	pj := filepath.Join(dir, "projects.json")

	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "data_dir preferred",
			body: `{"projects":[{"path":"/home/dev/proj","data_dir":"/home/dev/proj/.crush","last_accessed":"2026-07-09T00:00:00Z"}]}`,
			want: []string{"/home/dev/proj/.crush"},
		},
		{
			name: "path fallback when data_dir empty",
			body: `{"projects":[{"path":"/home/dev/other"}]}`,
			want: []string{filepath.Join("/home/dev/other", ".crush")},
		},
		{
			name: "malformed json yields nil",
			body: `{"projects":[`,
			want: nil,
		},
		{
			name: "empty entries skipped",
			body: `{"projects":[{"path":"","data_dir":""}]}`,
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := os.WriteFile(pj, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			got := readProjectDataDirs(pj)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("dir[%d] = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestReadProjectDataDirs_MissingFile(t *testing.T) {
	if got := readProjectDataDirs(filepath.Join(t.TempDir(), "absent.json")); got != nil {
		t.Errorf("missing file should yield nil, got %v", got)
	}
}
