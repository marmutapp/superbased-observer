package conversation

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/compression/conversation/types"
)

func TestDefaultRegistryCoversAllTypes(t *testing.T) {
	r := DefaultRegistry()
	for _, ct := range types.All() {
		if _, ok := r.Get(ct); !ok {
			t.Errorf("DefaultRegistry missing compressor for %q", ct)
		}
	}
}

func TestRegistryCompressDetectsAndDispatches(t *testing.T) {
	r := DefaultRegistry()

	// JSON-shaped body → JSON compressor. Needs to be big enough that the
	// schema replacement wins on bytes.
	body := []byte(`{"description":"long-enough-string-that-schema-wins","count":123456789,"items":[{"id":1111},{"id":2222},{"id":3333}]}`)
	out, ct, ok := r.Compress(body, "")
	if !ok {
		t.Fatalf("Compress returned ok=false for JSON body")
	}
	if ct != types.JSON {
		t.Fatalf("Compress detected type %q, want json", ct)
	}
	if len(out) >= len(body) {
		t.Fatalf("JSON compressor did not shrink body: in=%d out=%d", len(body), len(out))
	}

	// Unknown body → returned unchanged.
	empty := []byte(" \n ")
	out, ct, ok = r.Compress(empty, "")
	if ok {
		t.Fatalf("Compress returned ok=true for unknown body")
	}
	if ct != types.Unknown {
		t.Fatalf("Compress detected %q, want unknown", ct)
	}
	if string(out) != string(empty) {
		t.Fatalf("Compress mutated unknown body")
	}
}

func TestJSONCompressorReplacesScalarsWithTypeNames(t *testing.T) {
	c := NewJSONCompressor()
	// Inputs need to be meaty enough that the schema representation is
	// actually smaller than the original — the "don't grow" guard bails
	// on toy inputs where the type-name strings dominate.
	in := []byte(`{"name":"a-very-long-user-name-goes-right-here","count":1234567890,"ok":true,"x":null,"note":"this is some example description text that a tool might return"}`)
	out := c.Compress(in)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (out=%q)", err, out)
	}
	want := map[string]string{
		"name":  "<string>",
		"count": "<number>",
		"ok":    "<bool>",
		"x":     "<null>",
		"note":  "<string>",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("field %q = %v, want %v", k, got[k], v)
		}
	}
}

func TestJSONCompressorKeepsArrayElementSchemaAndLength(t *testing.T) {
	c := NewJSONCompressor()
	in := []byte(`{"items":[{"id":1111,"name":"alpha","role":"admin"},{"id":2222,"name":"beta","role":"user"},{"id":3333,"name":"gamma","role":"user"}]}`)
	out := c.Compress(in)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (out=%q)", err, out)
	}
	items, ok := got["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items got = %v (want single-element schema)", got["items"])
	}
	elem, ok := items[0].(map[string]any)
	if !ok || elem["id"] != "<number>" {
		t.Fatalf("array element schema = %v, want {id:<number>}", items[0])
	}
	if got["items#len"] != float64(3) {
		t.Fatalf("items#len = %v, want 3", got["items#len"])
	}
}

func TestJSONCompressorReturnsOriginalForInvalidJSON(t *testing.T) {
	c := NewJSONCompressor()
	in := []byte(`{not valid json}`)
	out := c.Compress(in)
	if string(out) != string(in) {
		t.Fatalf("invalid JSON mutated: %q -> %q", in, out)
	}
}

func TestLogsCompressorDedupesConsecutive(t *testing.T) {
	c := NewLogsCompressor()
	in := []byte("retry\nretry\nretry\nok\nretry\nretry\n")
	out := c.Compress(in)
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	want := []string{"retry [×3]", "ok", "retry [×2]"}
	if len(lines) != len(want) {
		t.Fatalf("lines = %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line[%d] = %q, want %q", i, lines[i], want[i])
		}
	}
}

func TestLogsCompressorClampsLargeInput(t *testing.T) {
	c := &LogsCompressor{opts: LogsOptions{MaxLines: 6, Head: 2, Tail: 2}}
	// 12 distinct lines so dedup is a no-op; truncation takes over.
	var b strings.Builder
	for i := 0; i < 12; i++ {
		b.WriteString("line")
		b.WriteByte(byte('a' + i))
		b.WriteByte('\n')
	}
	out := c.Compress([]byte(b.String()))
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("lines = %v, want 5 (2 head + marker + 2 tail)", lines)
	}
	if lines[0] != "linea" || lines[1] != "lineb" {
		t.Fatalf("head lines wrong: %v", lines[:2])
	}
	if !strings.Contains(lines[2], "8 lines elided") {
		t.Fatalf("marker missing elision count: %q", lines[2])
	}
	if lines[3] != "linek" || lines[4] != "linel" {
		t.Fatalf("tail lines wrong: %v", lines[3:])
	}
}

func TestTextCompressorHeadTail(t *testing.T) {
	c := NewTextCompressor(TextOptions{MaxLines: 6, Head: 2, Tail: 2})
	in := []byte("one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n")
	out := c.Compress(in)
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("lines = %v, want 5", lines)
	}
	if lines[0] != "one" || lines[1] != "two" {
		t.Fatalf("head lines wrong: %v", lines[:2])
	}
	if !strings.Contains(lines[2], "6 lines elided") {
		t.Fatalf("marker missing: %q", lines[2])
	}
	if lines[3] != "nine" || lines[4] != "ten" {
		t.Fatalf("tail lines wrong: %v", lines[3:])
	}
}

func TestTextCompressorShortInputPassesThrough(t *testing.T) {
	c := NewTextCompressor(TextOptions{})
	in := []byte("short\ntext\n")
	out := c.Compress(in)
	if string(out) != string(in) {
		t.Fatalf("short input mutated: in=%q out=%q", in, out)
	}
}

func TestDiffCompressorKeepsChangeLinesAndOneContext(t *testing.T) {
	c := NewDiffCompressor()
	in := []byte(`diff --git a/main.go b/main.go
index 111..222 100644
--- a/main.go
+++ b/main.go
@@ -1,30 +1,30 @@
 package main

 import (
 	"fmt"
 	"net/http"
 	"encoding/json"
 	"io"
 	"log"
 	"os"
 )

 type Server struct {
 	addr string
 	port int
 }

-func oldName() {
+func newName() {
 	fmt.Println("hi")
 }

 func handler(w http.ResponseWriter, r *http.Request) {
 	log.Println("got request")
 	data, err := io.ReadAll(r.Body)
 	if err != nil {
 		http.Error(w, err.Error(), 500)
 		return
 	}
 	log.Println(string(data))
 }
`)
	out := c.Compress(in)
	s := string(out)
	if len(s) >= len(in) {
		t.Fatalf("diff compressor did not shrink body: in=%d out=%d", len(in), len(s))
	}
	if !strings.Contains(s, "diff --git a/main.go b/main.go") {
		t.Errorf("missing git header: %s", s)
	}
	if !strings.Contains(s, "-func oldName() {") || !strings.Contains(s, "+func newName() {") {
		t.Errorf("missing change lines: %s", s)
	}
	if strings.Contains(s, `"net/http"`) {
		t.Errorf("far context line should have been stripped: %s", s)
	}
	if !strings.Contains(s, "context line") {
		t.Errorf("missing elision marker: %s", s)
	}
}

func TestHTMLCompressorStripsScriptAndStyle(t *testing.T) {
	c := NewHTMLCompressor()
	in := []byte(`<html><head><style>body{color:red}</style><script>alert(1)</script></head><body><p>hi</p></body></html>`)
	out := c.Compress(in)
	s := string(out)
	if strings.Contains(s, "color:red") {
		t.Errorf("style body leaked: %s", s)
	}
	if strings.Contains(s, "alert(1)") {
		t.Errorf("script body leaked: %s", s)
	}
	if !strings.Contains(s, "<p>hi</p>") {
		t.Errorf("visible content missing: %s", s)
	}
}

func TestHTMLCompressorStripsComments(t *testing.T) {
	c := NewHTMLCompressor()
	in := []byte(`<div><!-- internal note --><span>keep me</span></div>`)
	out := c.Compress(in)
	if strings.Contains(string(out), "internal note") {
		t.Errorf("comment leaked: %s", out)
	}
	if !strings.Contains(string(out), "keep me") {
		t.Errorf("visible span stripped: %s", out)
	}
}

func TestCodeCompressorKeepsSignaturesAndImports(t *testing.T) {
	c := NewCodeCompressor()
	in := []byte(`package main

import "fmt"
import "net/http"
import "encoding/json"

func greet(name string) string {
	msg := "hello, " + name
	msg = strings.ToUpper(msg)
	msg = strings.TrimSpace(msg)
	msg = strings.Replace(msg, " ", "_", -1)
	return msg
}

func fetchUser(id int) (*User, error) {
	resp, err := http.Get(fmt.Sprintf("/users/%d", id))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var u User
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, err
	}
	return &u, nil
}

func main() {
	fmt.Println(greet("world"))
	for i := 0; i < 10; i++ {
		fmt.Println(i)
	}
	for j := 0; j < 5; j++ {
		user, err := fetchUser(j)
		if err != nil {
			continue
		}
		fmt.Println(user.Name)
	}
}
`)
	out := c.Compress(in)
	s := string(out)
	if len(s) >= len(in) {
		t.Fatalf("compressor did not shrink body: in=%d out=%d", len(in), len(s))
	}
	if !strings.Contains(s, "import \"fmt\"") {
		t.Errorf("import dropped: %s", s)
	}
	if !strings.Contains(s, "func greet(") || !strings.Contains(s, "func main()") || !strings.Contains(s, "func fetchUser(") {
		t.Errorf("signatures dropped: %s", s)
	}
	if strings.Contains(s, "fmt.Println(i)") {
		t.Errorf("function body leaked: %s", s)
	}
	if !strings.Contains(s, "non-signature lines elided") {
		t.Errorf("missing elision marker: %s", s)
	}
}

func TestCodeCompressorFallsBackToTruncateWhenNoSignatures(t *testing.T) {
	c := NewCodeCompressor()
	var b strings.Builder
	for i := 0; i < 80; i++ {
		b.WriteString("x := 1 + 2\n")
	}
	out := c.Compress([]byte(b.String()))
	if len(out) >= b.Len() {
		t.Fatalf("fallback truncate didn't shrink: in=%d out=%d", b.Len(), len(out))
	}
	if !strings.Contains(string(out), "lines elided") {
		t.Errorf("no elision marker in fallback output: %s", out)
	}
}
