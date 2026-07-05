package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Guard evidence jobs (G2.3): `observer guard report | export |
// verify-audit` surfaced from the Security page through the SAME job
// registry + streaming subprocess runner the backfill Run-Now button
// uses (the prune/scan precedent — one registry, one poll endpoint,
// one timeout discipline). Argument discipline likewise inherited:
// every argv token comes from the closed allowlists below — nothing
// user-typed reaches os/exec.
//
// Artifacts: report and verify-audit are small text — they ride the
// job's captured output and download as a generated .txt. Exports can
// be large (full audit history), so the subprocess writes --out to
// <db-dir>/exports/ and the download endpoint streams that file.

// evidencePeriods is the closed window vocabulary the UI offers
// (values are durations export.ParsePeriod accepts).
var evidencePeriods = map[string]bool{
	"24h": true, "168h": true, "720h": true, "2160h": true, "8760h": true,
}

// evidenceFormats mirrors `guard export --format`.
var evidenceFormats = map[string]bool{"jsonl": true, "cef": true}

// evidenceSeverities mirrors `guard export --min-severity` ("" = all).
var evidenceSeverities = map[string]bool{
	"": true, "info": true, "warn": true, "high": true, "critical": true,
}

// handleGuardEvidence serves POST /api/guard/evidence — kick one
// evidence job. Body: {kind: report|export|verify-audit, period?,
// format?, min_severity?, json?}. Returns the job id immediately; the
// caller polls /api/backfill/jobs/<id> (the shared registry) and then
// downloads via /api/guard/evidence/download?job=<id>.
func (s *Server) handleGuardEvidence(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Kind        string `json:"kind"`
		Period      string `json:"period"`
		Format      string `json:"format"`
		MinSeverity string `json:"min_severity"`
		JSON        bool   `json:"json"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Period == "" {
		req.Period = "720h"
	}
	if !evidencePeriods[req.Period] {
		http.Error(w, "period must be one of 24h, 168h, 720h, 2160h, 8760h", http.StatusBadRequest)
		return
	}

	jobID, err := newJobID()
	if err != nil {
		writeErr(w, fmt.Errorf("generate job id: %w", err))
		return
	}

	var args []string
	outFile := ""
	switch req.Kind {
	case "report":
		args = []string{"guard", "report", "--period", req.Period}
		if req.JSON {
			args = append(args, "--json")
		}
	case "export":
		format := req.Format
		if format == "" {
			format = "jsonl"
		}
		if !evidenceFormats[format] {
			http.Error(w, "format must be jsonl or cef", http.StatusBadRequest)
			return
		}
		if !evidenceSeverities[req.MinSeverity] {
			http.Error(w, "min_severity must be one of info, warn, high, critical (empty = all)", http.StatusBadRequest)
			return
		}
		cfg, cfgErr := loadConfigForDashboard(s.opts.ConfigPath)
		if cfgErr != nil {
			writeErr(w, fmt.Errorf("load config: %w", cfgErr))
			return
		}
		dir := filepath.Join(filepath.Dir(cfg.Observer.DBPath), "exports")
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			writeErr(w, fmt.Errorf("ensure exports dir: %w", mkErr))
			return
		}
		outFile = filepath.Join(dir, "guard-export-"+jobID[:12]+"."+format)
		args = []string{"guard", "export", "--format", format, "--period", req.Period, "--out", outFile}
		if req.MinSeverity != "" {
			args = append(args, "--min-severity", req.MinSeverity)
		}
	case "verify-audit":
		args = []string{"guard", "verify-audit"}
	default:
		http.Error(w, `kind must be one of "report", "export", "verify-audit"`, http.StatusBadRequest)
		return
	}
	if s.opts.ConfigPath != "" {
		args = append(args, "--config", s.opts.ConfigPath)
	}

	job := &backfillJob{
		ID:        jobID,
		Mode:      "guard:" + req.Kind,
		Status:    "running",
		StartedAt: time.Now().UTC(),
		OutFile:   outFile,
	}
	s.backfillMu.Lock()
	s.backfillSeq++
	job.seq = s.backfillSeq
	s.backfillJobs[jobID] = job
	s.backfillMu.Unlock()

	go s.runBackfillJob(jobID, args)

	writeJSON(w, map[string]any{
		"job_id":     jobID,
		"mode":       job.Mode,
		"status":     "running",
		"started_at": job.StartedAt.Format(time.RFC3339),
	})
}

// handleGuardEvidenceDownload serves GET
// /api/guard/evidence/download?job=<id> — the artifact of a completed
// evidence job. File-backed jobs (export) stream their --out file;
// output-backed jobs (report, verify-audit) download the captured
// output as a generated text file. Failed jobs stay downloadable: a
// verify-audit that found a divergence "failed" in job terms, but the
// divergence detail IS the evidence. Only guard:* jobs resolve here —
// the endpoint is scoped to evidence, not a general job-output tap.
func (s *Server) handleGuardEvidenceDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("job")
	if id == "" {
		http.Error(w, "job query param required", http.StatusBadRequest)
		return
	}
	s.backfillMu.Lock()
	job, ok := s.backfillJobs[id]
	var snap backfillJob
	if ok {
		snap = *job
	}
	s.backfillMu.Unlock()
	if !ok || !strings.HasPrefix(snap.Mode, "guard:") {
		http.Error(w, "no such evidence job", http.StatusNotFound)
		return
	}
	if snap.Status == "running" {
		http.Error(w, "job still running — poll /api/backfill/jobs/"+id, http.StatusConflict)
		return
	}
	if snap.OutFile != "" {
		f, err := os.Open(snap.OutFile)
		if err != nil {
			writeErr(w, fmt.Errorf("open artifact: %w", err))
			return
		}
		defer f.Close()
		name := filepath.Base(snap.OutFile)
		ct := "text/plain; charset=utf-8"
		if strings.HasSuffix(name, ".jsonl") {
			ct = "application/x-ndjson"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		_, _ = io.Copy(w, f)
		return
	}
	name := strings.ReplaceAll(snap.Mode, ":", "-") + "-" + id[:8] + ".txt"
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, _ = io.WriteString(w, snap.Output)
}
