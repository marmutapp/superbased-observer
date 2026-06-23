package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
)

// newEvidenceServer seeds a config whose db_path lives in the test
// dir, so export artifacts land under <tdir>/exports — never the
// operator's real ~/.observer.
func newEvidenceServer(t *testing.T, fake backfillExecFn) (*Server, string) {
	t.Helper()
	tdir := t.TempDir()
	dbPath := filepath.Join(tdir, "d.db")
	cfgPath := filepath.Join(tdir, "config.toml")
	cfgToml := "[observer]\ndb_path = '" + filepath.ToSlash(dbPath) + "'\n"
	if err := os.WriteFile(cfgPath, []byte(cfgToml), 0o600); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	server, err := New(Options{DB: database, ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	server.execBackfill = fake
	return server, tdir
}

// kickEvidence POSTs /api/guard/evidence and returns the job id.
func kickEvidence(t *testing.T, s *Server, body string) string {
	t.Helper()
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/api/guard/evidence", strings.NewReader(body)))
	if rr.Code != 200 {
		t.Fatalf("POST evidence status = %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.JobID
}

// waitEvidenceJob polls the shared registry until the job leaves
// "running" (bounded — the fakes finish in milliseconds).
func waitEvidenceJob(t *testing.T, s *Server, id string) backfillJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr,
			httptest.NewRequest(http.MethodGet, "/api/backfill/jobs/"+id, nil))
		if rr.Code != 200 {
			t.Fatalf("poll status = %d", rr.Code)
		}
		var job backfillJob
		if err := json.Unmarshal(rr.Body.Bytes(), &job); err != nil {
			t.Fatalf("decode job: %v", err)
		}
		if job.Status != "running" {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job never left running")
	return backfillJob{}
}

// TestAPIGuardEvidence_Validation pins the closed vocabularies: every
// argv token comes from an allowlist, nothing user-typed reaches
// os/exec.
func TestAPIGuardEvidence_Validation(t *testing.T) {
	var called atomic.Bool
	fake := func(ctx context.Context, args []string, onChunk func([]byte)) (int, error) {
		called.Store(true)
		return 0, nil
	}
	s, _ := newEvidenceServer(t, fake)

	bad := []string{
		`{"kind":"bogus"}`,
		`{"kind":""}`,
		`{"kind":"report","period":"; rm -rf /"}`,
		`{"kind":"report","period":"99h"}`,
		`{"kind":"export","format":"xml"}`,
		`{"kind":"export","min_severity":"loud"}`,
	}
	for _, body := range bad {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr,
			httptest.NewRequest(http.MethodPost, "/api/guard/evidence", strings.NewReader(body)))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, rr.Code)
		}
	}
	if called.Load() {
		t.Error("exec must not run for any rejected request")
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/guard/evidence", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET evidence status = %d, want 405", rr.Code)
	}
}

// TestAPIGuardEvidence_ReportDownload covers the output-backed path:
// report text rides the job output and downloads as a generated .txt.
func TestAPIGuardEvidence_ReportDownload(t *testing.T) {
	res := &fakeExecResult{out: []byte("== Guard compliance report ==\nverdicts: 3\n"), exit: 0}
	s, _ := newEvidenceServer(t, newFakeExec(res))

	id := kickEvidence(t, s, `{"kind":"report","period":"168h"}`)
	job := waitEvidenceJob(t, s, id)
	if job.Status != "done" || job.Mode != "guard:report" {
		t.Fatalf("job = %+v, want done guard:report", job)
	}
	if len(res.sawArgs) < 4 || res.sawArgs[0] != "guard" || res.sawArgs[1] != "report" ||
		res.sawArgs[2] != "--period" || res.sawArgs[3] != "168h" {
		t.Errorf("args = %v, want guard report --period 168h …", res.sawArgs)
	}

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/api/guard/evidence/download?job="+id, nil))
	if rr.Code != 200 {
		t.Fatalf("download status = %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Guard compliance report") {
		t.Errorf("download body = %q, want the captured output", rr.Body.String())
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".txt") {
		t.Errorf("Content-Disposition = %q, want a .txt attachment", cd)
	}
}

// TestAPIGuardEvidence_ExportArtifact covers the file-backed path: the
// subprocess gets an --out under <db-dir>/exports and the download
// endpoint streams that artifact.
func TestAPIGuardEvidence_ExportArtifact(t *testing.T) {
	res := &fakeExecResult{exit: 0}
	var outArg atomic.Value
	fake := func(ctx context.Context, args []string, onChunk func([]byte)) (int, error) {
		res.sawArgs = append([]string(nil), args...)
		for i, a := range args {
			if a == "--out" && i+1 < len(args) {
				outArg.Store(args[i+1])
				// Stand in for the real export: write the artifact.
				if err := os.WriteFile(args[i+1], []byte(`{"rule_id":"R-101"}`+"\n"), 0o600); err != nil {
					return 1, err
				}
			}
		}
		return 0, nil
	}
	s, tdir := newEvidenceServer(t, fake)

	id := kickEvidence(t, s, `{"kind":"export","format":"jsonl","period":"720h","min_severity":"high"}`)
	job := waitEvidenceJob(t, s, id)
	if job.Status != "done" || job.OutFile == "" {
		t.Fatalf("job = %+v, want done with out_file", job)
	}
	got, _ := outArg.Load().(string)
	wantDir := filepath.Join(tdir, "exports")
	if filepath.Dir(got) != wantDir {
		t.Errorf("--out dir = %q, want %q", filepath.Dir(got), wantDir)
	}
	joined := strings.Join(res.sawArgs, " ")
	if !strings.Contains(joined, "--format jsonl") || !strings.Contains(joined, "--min-severity high") {
		t.Errorf("args = %v, want format + min-severity forwarded", res.sawArgs)
	}

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/api/guard/evidence/download?job="+id, nil))
	if rr.Code != 200 {
		t.Fatalf("download status = %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"rule_id":"R-101"`) {
		t.Errorf("download body = %q, want the artifact bytes", rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", ct)
	}
}

// TestAPIGuardEvidence_VerifyAuditFailureStillDownloads pins the
// honesty contract: a verify-audit that found a divergence exits 1
// (job "failed"), but the divergence detail IS the evidence — the
// download must still serve it.
func TestAPIGuardEvidence_VerifyAuditFailureStillDownloads(t *testing.T) {
	res := &fakeExecResult{out: []byte("audit chain BROKEN — first divergence at id 5\n"), exit: 1}
	s, _ := newEvidenceServer(t, newFakeExec(res))

	id := kickEvidence(t, s, `{"kind":"verify-audit"}`)
	job := waitEvidenceJob(t, s, id)
	if job.Status != "failed" {
		t.Fatalf("job status = %q, want failed on exit 1", job.Status)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/api/guard/evidence/download?job="+id, nil))
	if rr.Code != 200 {
		t.Fatalf("download status = %d, want 200 for a failed-but-informative job", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "BROKEN") {
		t.Errorf("download body = %q, want the divergence detail", rr.Body.String())
	}
}

// TestAPIGuardEvidence_DownloadGuards pins the scope rules: unknown
// job 404s, a running job 409s, and non-evidence registry jobs are
// not served by this endpoint.
func TestAPIGuardEvidence_DownloadGuards(t *testing.T) {
	res := &fakeExecResult{out: []byte("x"), exit: 0, delay: 300 * time.Millisecond}
	s, _ := newEvidenceServer(t, newFakeExec(res))

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/api/guard/evidence/download?job=nope", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown job status = %d, want 404", rr.Code)
	}

	id := kickEvidence(t, s, `{"kind":"report"}`)
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/api/guard/evidence/download?job="+id, nil))
	if rr.Code != http.StatusConflict {
		t.Errorf("running job download status = %d, want 409", rr.Code)
	}
	waitEvidenceJob(t, s, id)

	// A backfill-registry job is out of scope for the evidence tap.
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/api/backfill/run", strings.NewReader(`{"mode":"message-id"}`)))
	if rr.Code != 200 {
		t.Fatalf("backfill run status = %d", rr.Code)
	}
	var runResp struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &runResp); err != nil {
		t.Fatal(err)
	}
	waitEvidenceJob(t, s, runResp.JobID)
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/api/guard/evidence/download?job="+runResp.JobID, nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("backfill job via evidence download = %d, want 404", rr.Code)
	}
}
