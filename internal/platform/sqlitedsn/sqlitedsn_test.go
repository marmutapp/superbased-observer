package sqlitedsn

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	_ "modernc.org/sqlite"
)

func TestEscape(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain absolute path", "/home/user/.observer/observer.db", "/home/user/.observer/observer.db"},
		{"relative path", "testdata/state.db", "testdata/state.db"},
		{"empty", "", ""},
		{"question mark starts a query string", "/data/what?.db", "/data/what%3F.db"},
		{"hash starts a fragment", "/data/notes#1.db", "/data/notes%231.db"},
		{"percent is a URI escape introducer", "/data/100%.db", "/data/100%25.db"},
		{"percent escaped before its own output", "/data/a%3F.db", "/data/a%253F.db"},
		{"all three combined", "/d/a?b#c%d.db", "/d/a%3Fb%23c%25d.db"},
		{"ampersand and equals pass through", "/d/a&b=c.db", "/d/a&b=c.db"},
		{"spaces pass through", "/d/My Files/s.db", "/d/My Files/s.db"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Escape(tt.in); got != tt.want {
				t.Errorf("Escape(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestEscapeOpensHostilePath proves the whole point of the package:
// a database whose path contains URI-special characters opens
// read-only through a "file:" DSN built with Escape, and the query
// parameters after the path still apply.
func TestEscapeOpensHostilePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("? # are not legal in Windows file names")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "we?ird#100%.db")

	// Even a plain (non-URI) DSN is percent-decoded by the modernc
	// driver, so the create path needs Escape too.
	wdb, err := sql.Open("sqlite", "file:"+Escape(path))
	if err != nil {
		t.Fatalf("open for create: %v", err)
	}
	if _, err := wdb.Exec("CREATE TABLE t (v TEXT); INSERT INTO t VALUES ('ok')"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := wdb.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	dsn := "file:" + Escape(path) + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)"
	rdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open %q: %v", dsn, err)
	}
	defer rdb.Close()

	var v string
	if err := rdb.QueryRow("SELECT v FROM t").Scan(&v); err != nil {
		t.Fatalf("read back through escaped DSN: %v", err)
	}
	if v != "ok" {
		t.Fatalf("read back %q, want %q", v, "ok")
	}
	// mode=ro after the escaped path must still be honored.
	if _, err := rdb.Exec("INSERT INTO t VALUES ('nope')"); err == nil {
		t.Fatal("write succeeded through a mode=ro DSN — query params were lost")
	}

	// Sanity: the unescaped path really is broken (truncated at '?'),
	// so the escaping is load-bearing, not decorative.
	bad, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err == nil {
		if pingErr := bad.Ping(); pingErr == nil {
			// SQLite may lazily create "we" — it must not be our DB.
			if _, qErr := bad.Query("SELECT v FROM t"); qErr == nil {
				t.Fatal("unescaped DSN reached the hostile-named DB; test premise is wrong")
			}
		}
		_ = bad.Close()
	}
	_ = os.Remove(filepath.Join(dir, "we"))
}
