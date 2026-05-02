package codegraph

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	repoOwner  = "DeusData"
	repoName   = "codebase-memory-mcp"
	githubAPI  = "https://api.github.com"
	binaryName = "codebase-memory-mcp"
)

// InstallOptions configures Install.
type InstallOptions struct {
	// TargetDir is where the binary is installed. Defaults to ~/.local/bin.
	TargetDir string
	// HTTPClient overrides the default HTTP client (for tests).
	HTTPClient *http.Client
}

// Install downloads the latest codebase-memory-mcp release from GitHub,
// verifies the SHA256 checksum against checksums.txt, and extracts the
// platform-appropriate binary to TargetDir.
func Install(ctx context.Context, opts InstallOptions) (string, error) {
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	if opts.TargetDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("codegraph.Install: home dir: %w", err)
		}
		opts.TargetDir = filepath.Join(home, ".local", "bin")
	}

	rel, err := fetchLatestRelease(ctx, opts.HTTPClient)
	if err != nil {
		return "", err
	}

	archiveName := platformArchiveName()
	if archiveName == "" {
		return "", fmt.Errorf("codegraph.Install: unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	var archiveURL, checksumsURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case archiveName:
			archiveURL = a.BrowserDownloadURL
		case "checksums.txt":
			checksumsURL = a.BrowserDownloadURL
		}
	}
	if archiveURL == "" {
		return "", fmt.Errorf("codegraph.Install: asset %s not found in release %s", archiveName, rel.TagName)
	}
	if checksumsURL == "" {
		return "", fmt.Errorf("codegraph.Install: checksums.txt not found in release %s", rel.TagName)
	}

	checksums, err := downloadText(ctx, opts.HTTPClient, checksumsURL)
	if err != nil {
		return "", fmt.Errorf("codegraph.Install: download checksums: %w", err)
	}
	wantHash, err := parseChecksum(checksums, archiveName)
	if err != nil {
		return "", err
	}

	tmpFile, err := downloadToTemp(ctx, opts.HTTPClient, archiveURL)
	if err != nil {
		return "", fmt.Errorf("codegraph.Install: download archive: %w", err)
	}
	defer os.Remove(tmpFile)

	gotHash, err := fileHash(tmpFile)
	if err != nil {
		return "", fmt.Errorf("codegraph.Install: hash: %w", err)
	}
	if gotHash != wantHash {
		return "", fmt.Errorf("codegraph.Install: checksum mismatch for %s: got %s want %s", archiveName, gotHash, wantHash)
	}

	if err := os.MkdirAll(opts.TargetDir, 0o755); err != nil {
		return "", fmt.Errorf("codegraph.Install: mkdir %s: %w", opts.TargetDir, err)
	}

	binFile := binaryName
	if runtime.GOOS == "windows" {
		binFile += ".exe"
	}
	destPath := filepath.Join(opts.TargetDir, binFile)

	if strings.HasSuffix(archiveName, ".tar.gz") {
		if err := extractTarGz(tmpFile, binFile, destPath); err != nil {
			return "", fmt.Errorf("codegraph.Install: extract tar.gz: %w", err)
		}
	} else if strings.HasSuffix(archiveName, ".zip") {
		if err := extractZip(tmpFile, binFile, destPath); err != nil {
			return "", fmt.Errorf("codegraph.Install: extract zip: %w", err)
		}
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(destPath, 0o755); err != nil {
			return "", fmt.Errorf("codegraph.Install: chmod: %w", err)
		}
	}
	return destPath, nil
}

// DataDir returns the base directory where codebase-memory-mcp stores
// per-project graph databases.
func DataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", binaryName)
}

// FindProjectDB scans DataDir for a graph.db whose nodes table contains
// file_path entries matching projectRoot. Returns the path to the first
// match, or "" if no matching DB is found. This is a lightweight scan —
// each candidate DB gets a single probing query.
func FindProjectDB(projectRoot string) string {
	dir := DataDir()
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(projectRoot)
	if err != nil {
		abs = projectRoot
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dbPath := filepath.Join(dir, entry.Name(), "graph.db")
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		if probeProjectMatch(dbPath, abs) {
			return dbPath
		}
	}
	return ""
}

func probeProjectMatch(dbPath, absProjectRoot string) bool {
	dsn := fmt.Sprintf("file:%s?mode=ro&immutable=1", dbPath)
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return false
	}
	defer d.Close()
	var n int
	prefix := absProjectRoot + string(filepath.Separator)
	err = d.QueryRow(
		`SELECT 1 FROM nodes WHERE file_path = ? OR file_path LIKE ? LIMIT 1`,
		absProjectRoot, prefix+"%",
	).Scan(&n)
	return err == nil && n == 1
}

type releaseInfo struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func fetchLatestRelease(ctx context.Context, client *http.Client) (releaseInfo, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", githubAPI, repoOwner, repoName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return releaseInfo{}, fmt.Errorf("codegraph.Install: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return releaseInfo{}, fmt.Errorf("codegraph.Install: fetch release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return releaseInfo{}, fmt.Errorf("codegraph.Install: GitHub API returned %d", resp.StatusCode)
	}
	var rel releaseInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return releaseInfo{}, fmt.Errorf("codegraph.Install: parse release: %w", err)
	}
	return rel, nil
}

func platformArchiveName() string {
	os := runtime.GOOS
	arch := runtime.GOARCH
	switch {
	case os == "linux" && arch == "amd64":
		return binaryName + "-linux-amd64.tar.gz"
	case os == "linux" && arch == "arm64":
		return binaryName + "-linux-arm64.tar.gz"
	case os == "darwin" && arch == "arm64":
		return binaryName + "-darwin-arm64.tar.gz"
	case os == "darwin" && arch == "amd64":
		return binaryName + "-darwin-amd64.tar.gz"
	case os == "windows" && arch == "amd64":
		return binaryName + "-windows-amd64.zip"
	}
	return ""
}

func downloadText(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func parseChecksum(checksums, filename string) (string, error) {
	for _, line := range strings.Split(checksums, "\n") {
		parts := strings.Fields(line)
		if len(parts) >= 2 && parts[1] == filename {
			return strings.ToLower(parts[0]), nil
		}
	}
	return "", fmt.Errorf("codegraph.Install: no checksum for %s in checksums.txt", filename)
}

func downloadToTemp(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %d", url, resp.StatusCode)
	}
	tmp, err := os.CreateTemp("", "codegraph-install-*")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func extractTarGz(archivePath, targetName, destPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		base := filepath.Base(hdr.Name)
		if base == targetName && hdr.Typeflag == tar.TypeReg {
			out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				return err
			}
			defer out.Close()
			if _, err := io.Copy(out, io.LimitReader(tr, 256<<20)); err != nil {
				return err
			}
			return nil
		}
	}
	return fmt.Errorf("binary %s not found in archive", targetName)
}

func extractZip(archivePath, targetName, destPath string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		base := filepath.Base(f.Name)
		if base == targetName && !f.FileInfo().IsDir() {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			defer rc.Close()
			out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				return err
			}
			defer out.Close()
			if _, err := io.Copy(out, io.LimitReader(rc, 256<<20)); err != nil {
				return err
			}
			return nil
		}
	}
	return fmt.Errorf("binary %s not found in archive", targetName)
}
