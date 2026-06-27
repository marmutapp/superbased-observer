package antigravity

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/platform/protowire"
	_ "modernc.org/sqlite"
)

// parseCLIDB reads a newer Antigravity CLI conversation store —
// .gemini/antigravity-cli/conversations/<uuid>.db. Unlike the desktop /
// older-CLI .pb path, the SQLite blobs are PLAINTEXT protobuf (verified
// 2026-06-26), so no OSCrypt decryption or gRPC bridge is needed: we read
// gen_metadata directly and emit one TokenEvent per generation.
//
// Each gen_metadata.data blob is the per-generation message (the same shape
// the structured .pb decoder sees as `1.3[]`), so its usage submessage at
// path 1.17.2.X uses the IDENTICAL field map as structured.go's
// `1.3.1.17.2.X` (1=input, 2=cacheCreation, 5=cacheRead, 9=reasoning,
// 10=output; .3 == .9 + .10, confirmed in live data). The model id is the
// string at 1.19. This reuses the documented mapping rather than guessing.
func (a *Adapter) parseCLIDB(ctx context.Context, path string, size int64) (adapter.ParseResult, error) {
	res := adapter.ParseResult{NewOffset: size}

	conversationID := uuidFromFilename(path)
	if conversationID == "" {
		return res, nil
	}
	projectRoot := "[antigravity]"
	if idx := a.lookupCLIIndexEntry(path, conversationID); idx != nil && idx.workspaceURI != "" {
		if root := decodeFileURIToRoot(idx.workspaceURI); root != "" {
			projectRoot = root
		}
	}

	dsn := fmt.Sprintf("file:%s?mode=ro&immutable=1&_pragma=busy_timeout(2000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return res, fmt.Errorf("antigravity.parseCLIDB: open: %w", err)
	}
	defer db.Close()

	// The .db carries its own conversation→project binding in
	// trajectory_metadata_blob (field 18 = project id), which resolves to the
	// workspace folder via ~/.gemini/config/projects/<id>.json. This is more
	// reliable for the .db layout than lookupCLIIndexEntry's cli-*.log regex
	// (which depends on log retention and only matches UUID project ids), so
	// it fills the gap when the log path missed. A no-workspace conversation
	// (the default-cli-project) resolves to "" here and is left for the
	// transcript-metadata recovery in augmentResultFromHistory below.
	if projectRoot == "[antigravity]" {
		if root := projectRootFromTrajectoryMeta(ctx, db, path); root != "" {
			projectRoot = root
		}
	}

	rows, err := db.QueryContext(ctx, "SELECT idx, data FROM gen_metadata ORDER BY idx")
	if err != nil {
		// Schema may evolve / table absent — degrade to no rows rather
		// than failing the whole parse.
		return res, nil
	}
	defer rows.Close()

	for rows.Next() {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		var idx int64
		var data []byte
		if err := rows.Scan(&idx, &data); err != nil {
			continue
		}
		gen := decodeGenMetadata(data)
		if gen.input == 0 && gen.output == 0 && gen.cacheRead == 0 && gen.cacheCreation == 0 && gen.reasoning == 0 {
			continue
		}
		res.TokenEvents = append(res.TokenEvents, models.TokenEvent{
			SourceFile:          path,
			SourceEventID:       fmt.Sprintf("antigravity-cli-db:%s:gen:%d", conversationID, idx),
			SessionID:           conversationID,
			ProjectRoot:         projectRoot,
			Tool:                models.ToolAntigravity,
			Model:               gen.model,
			InputTokens:         int64(gen.input),
			OutputTokens:        int64(gen.output),
			CacheReadTokens:     int64(gen.cacheRead),
			CacheCreationTokens: int64(gen.cacheCreation),
			ReasoningTokens:     int64(gen.reasoning),
			Source:              models.TokenSourceJSONL,
			Reliability:         models.ReliabilityApproximate,
			MessageID:           fmt.Sprintf("%s:gen:%d", conversationID, idx),
		})
	}
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("antigravity.parseCLIDB: scan: %w", err)
	}

	// gen_metadata yields only token usage — recover the conversation TEXT
	// (user prompts, assistant/planner responses, tool calls) from the sibling
	// brain/<uuid>/.system_generated/logs/transcript.jsonl, exactly as the .pb
	// path does at every event-producing exit. Without this the dashboard
	// shows token rows with no message body ("API call (no recovered text)").
	// augmentResultFromHistory also patches the project root from the
	// transcript's ADDITIONAL_METADATA when projectRoot is still the
	// "[antigravity]" placeholder (workspace-stamped sessions).
	a.augmentResultFromHistory(path, conversationID, projectRoot, &res)
	return res, nil
}

// projectRootFromTrajectoryMeta resolves a CLI conversation's workspace root
// from the .db's own trajectory_metadata_blob: field 18 is the project id,
// which maps to a workspace folder via ~/.gemini/config/projects/<id>.json
// (projectResources.resources[].gitFolder.folderUri). Returns "" when the
// blob/field/project-file is absent or the project has no workspace folder
// (e.g. the default-cli-project), so the caller falls back.
func projectRootFromTrajectoryMeta(ctx context.Context, db *sql.DB, sessionPath string) string {
	var data []byte
	row := db.QueryRowContext(ctx, "SELECT data FROM trajectory_metadata_blob LIMIT 1")
	if err := row.Scan(&data); err != nil || len(data) == 0 {
		return ""
	}
	projectID := projectIDFromTrajectoryBlob(data)
	if projectID == "" {
		return ""
	}
	_, geminiRoot := cliRootsFor(sessionPath)
	if geminiRoot == "" {
		return ""
	}
	proj, ok := readCLIProjectFile(filepath.Join(geminiRoot, "config", "projects", projectID+".json"))
	if !ok {
		return ""
	}
	if len(proj.ProjectResources.Resources) > 0 {
		if uri := proj.ProjectResources.Resources[0].GitFolder.FolderURI; uri != "" {
			return decodeFileURIToRoot(uri)
		}
	}
	return ""
}

// projectIDFromTrajectoryBlob walks a trajectory_metadata_blob protobuf and
// returns the top-level field 18 (project id) string. protowire.Walk yields
// top-level fields before recursing and treats malformed nested payloads as
// soft failures, so field 18 is reached even past the opaque field 15.
func projectIDFromTrajectoryBlob(data []byte) string {
	var id string
	_ = protowire.Walk(data, func(f protowire.Field) error {
		if id == "" && len(f.Path) == 1 && f.Path[0] == 18 &&
			f.WireType == protowire.WireBytes && protowire.IsLikelyText(f.Bytes) {
			id = string(f.Bytes)
		}
		return nil
	})
	return id
}

// genUsage holds the per-generation model + token counts decoded from a
// gen_metadata.data protobuf blob.
type genUsage struct {
	model         string
	input         uint64
	output        uint64
	cacheRead     uint64
	cacheCreation uint64
	reasoning     uint64
}

// decodeGenMetadata walks a gen_metadata.data blob and pulls the model id
// (1.19) and the usage counts (1.17.2.X) using the same field map the
// structured .pb decoder applies to 1.3.1.17.2.X.
func decodeGenMetadata(buf []byte) genUsage {
	var g genUsage
	_ = protowire.Walk(buf, func(f protowire.Field) error {
		switch {
		case pathEq(f.Path, 1, 19) && f.WireType == protowire.WireBytes:
			if g.model == "" && protowire.IsLikelyText(f.Bytes) {
				g.model = string(f.Bytes)
			}
		case len(f.Path) == 4 && pathPrefix(f.Path, 1, 17, 2) && f.WireType == protowire.WireVarint:
			switch f.Path[3] {
			case 1:
				g.input = f.Varint
			case 2:
				g.cacheCreation = f.Varint
			case 5:
				g.cacheRead = f.Varint
			case 9:
				g.reasoning = f.Varint
			case 10:
				g.output = f.Varint
			}
		}
		return nil
	})
	return g
}
