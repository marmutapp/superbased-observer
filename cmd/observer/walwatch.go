package main

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// walwatch.go — §1e follow-through (2026-08-22 write-stall arc). The
// 2026-08-21 DB write stall left observer.db-wal pinned at 15.5 GB for
// hours: every frame was long since checkpointed into the main DB (the
// eventual TRUNCATE returned 0|0|0) but the file was never RELEASED,
// because a long-lived reader (the MCP `observer serve` process, up on a
// deleted binary) plausibly held a read transaction open across the
// checkpoint window. This watchdog makes that state visible and
// self-healing instead of silent: on a bounded cadence it stats the WAL
// file, WARNs once it exceeds [observer.retention].wal_alert_mb, and
// attempts `PRAGMA wal_checkpoint(TRUNCATE)` through its own connection.
// The attempt is best-effort by design — a busy checkpoint returns
// SQLITE_BUSY row counts without disturbing readers; when no reader pins
// the WAL it reclaims the file outright. It does NOT chase the serve-side
// read-txn holder itself: that needs a cross-process story (restart the
// stale `serve`), which stays an operator action the alert names.

// walWatchdogLoop runs until ctx is cancelled. Fail-soft like its
// retention sibling: any setup failure logs and exits the loop without
// touching proxy/watcher/dashboard health.
func walWatchdogLoop(ctx context.Context, configPath string) {
	cfg, database, cleanup, err := loadConfigAndDB(ctx, configPath)
	if err != nil {
		return
	}
	defer cleanup()
	logger := newLogger(cfg.Observer.LogLevel)

	alertBytes := int64(cfg.Observer.Retention.WALAlertMB) * 1024 * 1024
	interval, enabled := walWatchdogInterval(
		cfg.Observer.Retention.WALAlertMB,
		cfg.Observer.Retention.WALWatchMinutes,
	)
	if !enabled {
		return // either documented disable switch is off
	}

	walPath := cfg.Observer.DBPath + "-wal"
	if cfg.Observer.DBPath == "" {
		// Same default resolution loadConfigAndDB applied to the handle;
		// derive from the config the same way prune.go does. If still
		// empty there is nothing to watch.
		if home, herr := os.UserHomeDir(); herr == nil {
			walPath = filepath.Join(home, ".observer", "observer.db-wal")
		} else {
			return
		}
	}

	check := func() {
		fi, err := os.Stat(walPath)
		if err != nil {
			return // no WAL yet (fresh DB) or unreadable: nothing to alert on
		}
		if fi.Size() < alertBytes {
			return
		}
		logger.Warn("WAL file exceeds alert threshold — attempting TRUNCATE checkpoint (§1e)",
			"wal_bytes", fi.Size(),
			"alert_mb", cfg.Observer.Retention.WALAlertMB,
			"hint", "a busy result means a live reader (e.g. an old `observer serve`) holds a read txn — restart it")
		var busy, logged, ckpt int64
		if err := database.QueryRowContext(ctx,
			`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logged, &ckpt); err != nil {
			logger.Warn("wal_checkpoint(TRUNCATE) failed", "err", err)
			return
		}
		if fi2, err := os.Stat(walPath); err == nil {
			logger.Info("wal_checkpoint(TRUNCATE) result",
				"busy", busy, "logged", logged, "checkpointed", ckpt,
				"wal_bytes_after", fi2.Size())
		}
	}

	check() // startup pass — catches a WAL that grew while the daemon was down
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

// walWatchdogInterval resolves the watchdog's two documented disable
// switches without touching I/O. Defaults are applied by config loading; a
// non-positive explicit cadence must therefore stay disabled here.
func walWatchdogInterval(alertMB, watchMinutes int) (time.Duration, bool) {
	if alertMB <= 0 || watchMinutes <= 0 {
		return 0, false
	}
	return time.Duration(watchMinutes) * time.Minute, true
}
