// Command sbo-capture is a DEV-ONLY diagnostic. It attaches to a running
// Chrome over the Chrome DevTools Protocol (CDP) and records the completion
// request/response shapes of ChatGPT, Claude, Perplexity, Gemini (+ Copilot,
// bonus) so the browser extension's per-site parsers (parsers.js) can be
// validated against REAL authenticated traffic — WITHOUT MITM and WITHOUT
// installing the extension.
//
// It makes NO outbound network calls: it talks ONLY to the local Chrome
// DevTools endpoint on 127.0.0.1:<port>. All text samples are truncated at the
// source and the dumps are operator-redactable. See README.md.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// coordinator holds shared run state across tab watchers.
type coordinator struct {
	outDir string
	once   bool
	urls   *urlLog // cross-target request/WebSocket diagnostic (_urls.json)

	mu       sync.Mutex
	captured map[string]bool // sites that have produced ≥1 dump
	allDone  chan struct{}
	attached int
}

func (c *coordinator) notify(site string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.captured[site] {
		return
	}
	c.captured[site] = true
	if c.once && len(c.captured) >= c.attached && c.attached > 0 {
		select {
		case <-c.allDone:
		default:
			close(c.allDone)
		}
	}
}

func main() {
	port := flag.Int("port", 9222, "Chrome remote-debugging port")
	outDir := flag.String("out", "", "output directory for dumps (default: a temp dir)")
	once := flag.Bool("once", false, "capture one turn per site, then exit")
	flag.Parse()

	dir := *outDir
	if dir == "" {
		d, err := os.MkdirTemp("", "sbo-dumps-")
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot create temp dir: %v\n", err)
			os.Exit(1)
		}
		dir = d
	}

	fmt.Println("sbo-capture — CDP completion-shape recorder (DEV TOOL)")
	fmt.Println(privacyNote)
	fmt.Printf("port=%d  out=%s  once=%v\n\n", *port, dir, *once)

	targets, err := discoverTargets(*port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot reach Chrome DevTools on 127.0.0.1:%d — is Chrome running with --remote-debugging-port=%d ?\n  error: %v\n", *port, *port, err)
		os.Exit(1)
	}

	coord := &coordinator{
		outDir:   dir,
		once:     *once,
		urls:     newURLLog(),
		captured: map[string]bool{},
		allDone:  make(chan struct{}),
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	attachedSites := map[string]bool{}

	for _, t := range targets {
		if t.Type != "page" || t.WebSocketDebuggerURL == "" {
			continue
		}
		s := siteForURL(t.URL)
		if s == nil {
			continue
		}
		client, err := dialCDP(t.WebSocketDebuggerURL)
		if err != nil {
			fmt.Printf("  [warn] could not attach to %s (%s): %v\n", s.Site, shorten(t.URL, 60), err)
			continue
		}
		fmt.Printf("attached: %-16s tab=%s\n", s.Site, shorten(t.URL, 70))
		attachedSites[s.Site] = true
		w := newTabWatcher(s, t.URL, client, coord)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer client.Close()
			w.run(stop)
		}()
	}

	coord.mu.Lock()
	coord.attached = len(attachedSites)
	coord.mu.Unlock()

	if len(attachedSites) == 0 {
		fmt.Println("\nNo target tabs found. Open chatgpt.com / claude.ai / perplexity.ai / gemini.google.com")
		fmt.Println("(and optionally copilot.microsoft.com) in the debugged Chrome, sign in, then re-run.")
		os.Exit(1)
	}

	fmt.Printf("\n================================================================\n")
	fmt.Printf(" WAITING — send ONE message in each attached tab now.\n")
	fmt.Printf(" Attached sites: %d.  Dumps land in: %s\n", len(attachedSites), dir)
	if *once {
		fmt.Printf(" --once: exits after each attached site captures one turn.\n")
	} else {
		fmt.Printf(" Keep-running: press Ctrl-C to stop (dumps written as they arrive).\n")
	}
	fmt.Printf("================================================================\n\n")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	select {
	case <-sig:
		fmt.Println("\ninterrupted — stopping; dumps already written are in", dir)
	case <-coord.allDone:
		fmt.Println("\nall attached sites captured — exiting.")
	}
	close(stop)

	// Give watchers a moment to finish any in-flight dump write.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
	// Always emit the cross-target URL/WebSocket diagnostic, even on an empty
	// run — it's the map that reveals the real conduit host/path to tune against.
	if path, err := coord.urls.write(dir); err != nil {
		fmt.Printf("  [warn] could not write URL diagnostic: %v\n", err)
	} else {
		fmt.Println("url diagnostic:", path)
	}
	fmt.Println("dumps directory:", dir)
}

// discoverTargets fetches the open tabs from the Chrome DevTools JSON endpoint
// (local only — 127.0.0.1).
func discoverTargets(port int) ([]cdpTarget, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/json", port)
	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var targets []cdpTarget
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return nil, fmt.Errorf("decode /json: %w", err)
	}
	return targets, nil
}
