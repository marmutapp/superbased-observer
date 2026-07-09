package goose

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// fixtureDB is built ONCE per test binary by TestMain. Repo convention:
// SQLite fixtures are synthesized in-test (no .db binary is tracked — the
// tree-wide *.db gitignore is deliberate); fixtureSQL below is the
// anonymized dump derived from a live goose 1.41.0 capture (WSL,
// 2026-07-09). Working-dir paths and message text are neutralized; ONE
// Windows-style C:\… working_dir row is kept to pin the crossmount lane,
// and all structural shapes are preserved: text / toolRequest /
// toolResponse-with-structuredContent, GROSS input, NULL cache_write, and
// two token-EMPTY provider-error sessions.
var fixtureDB string

// fixtureDB2 is a SECOND store whose session id COLLIDES with the first
// store's `20260708_1` — the real-environment shape where a WSL store and
// a Windows store each independently generate `YYYYMMDD_seq` ids. Pins the
// store-scoped SessionID lane (scopedSessionID): parsing both stores must
// yield two DISTINCT sessions, never one merged row.
var fixtureDB2 string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "goose-fixture")
	if err != nil {
		panic(err)
	}
	fixtureDB = filepath.Join(dir, "sessions", "sessions.db")
	fixtureDB2 = filepath.Join(dir, "second", "sessions", "sessions.db")
	if err := buildFixture(fixtureDB, fixtureSQL); err != nil {
		panic(err)
	}
	if err := buildFixture(fixtureDB2, fixtureSQL2); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func buildFixture(path, ddl string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	if _, err := db.Exec(ddl); err != nil {
		db.Close()
		return err
	}
	return db.Close()
}

const fixtureSQL = `
CREATE TABLE sessions (
  id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
  user_set_name BOOLEAN DEFAULT FALSE, session_type TEXT NOT NULL DEFAULT 'user',
  working_dir TEXT NOT NULL, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, extension_data TEXT DEFAULT '{}',
  total_tokens INTEGER, input_tokens INTEGER, output_tokens INTEGER,
  cache_read_tokens INTEGER, cache_write_tokens INTEGER,
  accumulated_total_tokens INTEGER, accumulated_input_tokens INTEGER,
  accumulated_output_tokens INTEGER, accumulated_cache_read_tokens INTEGER,
  accumulated_cache_write_tokens INTEGER, accumulated_cost REAL,
  schedule_id TEXT, recipe_json TEXT, user_recipe_values_json TEXT,
  provider_name TEXT, model_config_json TEXT, goose_mode TEXT NOT NULL DEFAULT 'auto',
  archived_at TIMESTAMP, project_id TEXT);

-- Windows-side, token-bearing (openai/gpt-4o), 2 text turns.
INSERT INTO sessions (id, session_type, working_dir, created_at, updated_at,
  total_tokens, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
  accumulated_total_tokens, accumulated_input_tokens, accumulated_output_tokens,
  accumulated_cache_read_tokens, accumulated_cache_write_tokens, accumulated_cost,
  provider_name, model_config_json, goose_mode)
VALUES ('20260708_1','user','C:\Users\dev\project','2026-07-08 21:12:19','2026-07-08 21:12:47',
  4328,4081,247,3968,NULL,8391,8134,257,3968,NULL,0.017945,'openai',
  '{"model_name":"gpt-4o","context_limit":128000}','auto');

-- Token-EMPTY provider-error rows (openrouter), one user text each.
INSERT INTO sessions (id, session_type, working_dir, created_at, updated_at, provider_name, model_config_json, goose_mode)
VALUES ('20260709_1','user','/home/user/scratch','2026-07-09 00:08:18','2026-07-09 00:08:19','openrouter','{"model_name":"anthropic/claude-sonnet-4","context_limit":1000000}','auto');
INSERT INTO sessions (id, session_type, working_dir, created_at, updated_at, provider_name, model_config_json, goose_mode)
VALUES ('20260709_2','user','/home/user/scratch','2026-07-09 00:08:53','2026-07-09 00:08:54','openrouter','{"model_name":"anthropic/claude-sonnet-4","context_limit":1000000}','auto');

-- Native Linux, token-bearing (openai/gpt-4o), cache_read=0, 1 text turn.
INSERT INTO sessions (id, session_type, working_dir, created_at, updated_at,
  total_tokens, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
  accumulated_total_tokens, accumulated_input_tokens, accumulated_output_tokens,
  accumulated_cache_read_tokens, accumulated_cache_write_tokens, accumulated_cost,
  provider_name, model_config_json, goose_mode)
VALUES ('20260709_3','user','/home/user/project','2026-07-09 07:35:05','2026-07-09 07:35:12',
  4091,4053,38,0,NULL,4091,4053,38,0,NULL,0.0105125,'openai',
  '{"model_name":"gpt-4o","context_limit":128000}','auto');

-- Tool-call session (openai/gpt-4o-mini): write + shell, GROSS input w/ cache_read.
INSERT INTO sessions (id, session_type, working_dir, created_at, updated_at,
  total_tokens, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
  accumulated_total_tokens, accumulated_input_tokens, accumulated_output_tokens,
  accumulated_cache_read_tokens, accumulated_cache_write_tokens, accumulated_cost,
  provider_name, model_config_json, goose_mode)
VALUES ('20260709_4','user','/home/user/keyed','2026-07-09 09:46:40','2026-07-09 09:46:45',
  3157,3125,32,2944,NULL,6275,6193,82,2944,NULL,0.00075735,'openai',
  '{"model_name":"gpt-4o-mini","context_limit":128000}','auto');

-- Single-turn GROSS-input proof (input 3062 vs cache_read 2944 -> net 118).
INSERT INTO sessions (id, session_type, working_dir, created_at, updated_at,
  total_tokens, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
  accumulated_total_tokens, accumulated_input_tokens, accumulated_output_tokens,
  accumulated_cache_read_tokens, accumulated_cache_write_tokens, accumulated_cost,
  provider_name, model_config_json, goose_mode)
VALUES ('20260709_5','user','/home/user/keyed','2026-07-09 09:47:49','2026-07-09 09:47:51',
  3064,3062,2,2944,NULL,3064,3062,2,2944,NULL,0.0002397,'openai',
  '{"model_name":"gpt-4o-mini","context_limit":128000}','auto');

-- Synthetic edge case: a session whose only message is malformed JSON.
INSERT INTO sessions (id, session_type, working_dir, created_at, updated_at, provider_name, model_config_json, goose_mode)
VALUES ('20260709_6','user','/home/user/keyed','2026-07-09 09:50:00','2026-07-09 09:50:01','openai','{"model_name":"gpt-4o-mini"}','auto');

CREATE TABLE messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT, message_id TEXT, session_id TEXT NOT NULL,
  role TEXT NOT NULL, content_json TEXT NOT NULL, created_timestamp INTEGER NOT NULL,
  timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP, tokens INTEGER, metadata_json TEXT);

INSERT INTO messages (id, message_id, session_id, role, content_json, created_timestamp, tokens) VALUES
 (1,'msg_a','20260708_1','user','[{"type":"text","text":"hello"}]',1783545150,NULL),
 (2,'chatcmpl-a','20260708_1','assistant','[{"type":"text","text":"Hello! How can I assist you today?"}]',1783545153,NULL),
 (3,'msg_b','20260708_1','user','[{"type":"text","text":"Can you give me a quick summary of the project?"}]',1783545163,NULL),
 (4,'chatcmpl-b','20260708_1','assistant','[{"type":"text","text":"The project is a Go tool that analyzes AI coding activity. Let me know if you want a deeper dive."}]',1783545164,NULL),
 (5,'msg_c','20260709_1','user','[{"type":"text","text":"hello"}]',1783555699,NULL),
 (6,'msg_d','20260709_2','user','[{"type":"text","text":"hi"}]',1783555734,NULL),
 (7,'msg_e','20260709_3','user','[{"type":"text","text":"Can you give me a quick summary of the project?"}]',1783582508,NULL),
 (8,'chatcmpl-e','20260709_3','assistant','[{"type":"text","text":"Hello! How can I assist you today? Feel free to ask about the project."}]',1783582511,NULL),
 (9,'msg_f','20260709_4','user','[{"type":"text","text":"Create a file hello.txt containing exactly: hello from goose. Then run ls."}]',1783590401,NULL),
 (10,'msg_g','20260709_4','assistant','[{"type":"toolRequest","id":"call_write1","toolCall":{"status":"success","value":{"name":"write","arguments":{"path":"hello.txt","content":"hello from goose."}}},"_meta":{"goose_extension":"developer"}}]',1783590404,NULL),
 (11,'msg_h','20260709_4','user','[{"type":"toolResponse","id":"call_write1","toolResult":{"status":"success","value":{"content":[{"type":"text","text":"Created hello.txt (1 lines)","annotations":{"priority":0.0}}],"isError":false}}}]',1783590404,NULL),
 (12,'msg_i','20260709_4','assistant','[{"type":"toolRequest","id":"call_shell1","toolCall":{"status":"success","value":{"name":"shell","arguments":{"command":"ls"}}},"_meta":{"goose_extension":"developer"}}]',1783590404,NULL),
 (13,'msg_j','20260709_4','user','[{"type":"toolResponse","id":"call_shell1","toolResult":{"status":"success","value":{"content":[{"type":"text","text":"hello.txt","annotations":{"priority":0.0}}],"structuredContent":{"stdout":"hello.txt","stderr":"","exit_code":0},"isError":false}}}]',1783590404,NULL),
 (14,'chatcmpl-k','20260709_4','assistant','[{"type":"text","text":"I created the file hello.txt with the requested text and listed the directory."}]',1783590405,NULL),
 (15,'msg_l','20260709_5','user','[{"type":"text","text":"Reply with exactly the word READY and nothing else."}]',1783590470,NULL),
 (16,'chatcmpl-m','20260709_5','assistant','[{"type":"text","text":"READY"}]',1783590471,NULL),
 (17,'msg_n','20260709_6','user','{ this is not valid json',1783590600,NULL);

CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at TIMESTAMP);
INSERT INTO schema_version VALUES (14,'2026-07-08 21:12:19');
`

// fixtureSQL2 mirrors the operator's SECOND (Windows-side) store: its
// `20260708_1` id collides with the first store's, and it carries one
// extra non-colliding session. Token-empty (the live Windows rows were),
// so it contributes tool events only.
const fixtureSQL2 = `
CREATE TABLE sessions (
  id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
  user_set_name BOOLEAN DEFAULT FALSE, session_type TEXT NOT NULL DEFAULT 'user',
  working_dir TEXT NOT NULL, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, extension_data TEXT DEFAULT '{}',
  total_tokens INTEGER, input_tokens INTEGER, output_tokens INTEGER,
  cache_read_tokens INTEGER, cache_write_tokens INTEGER,
  accumulated_total_tokens INTEGER, accumulated_input_tokens INTEGER,
  accumulated_output_tokens INTEGER, accumulated_cache_read_tokens INTEGER,
  accumulated_cache_write_tokens INTEGER, accumulated_cost REAL,
  schedule_id TEXT, recipe_json TEXT, user_recipe_values_json TEXT,
  provider_name TEXT, model_config_json TEXT, goose_mode TEXT NOT NULL DEFAULT 'auto',
  archived_at TIMESTAMP, project_id TEXT);

-- COLLIDES with store 1's 20260708_1 (different machine, same date+seq).
INSERT INTO sessions (id, session_type, working_dir, created_at, updated_at, provider_name, model_config_json, goose_mode)
VALUES ('20260708_1','user','C:\Users\dev','2026-07-08 22:00:00','2026-07-08 22:00:05','openai','{"model_name":"gpt-4o"}','auto');
INSERT INTO sessions (id, session_type, working_dir, created_at, updated_at, provider_name, model_config_json, goose_mode)
VALUES ('20260708_2','user','C:\Users\dev','2026-07-08 22:10:00','2026-07-08 22:10:05','openai','{"model_name":"gpt-4o"}','auto');

CREATE TABLE messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT, message_id TEXT, session_id TEXT NOT NULL,
  role TEXT NOT NULL, content_json TEXT NOT NULL, created_timestamp INTEGER NOT NULL,
  timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP, tokens INTEGER, metadata_json TEXT);

INSERT INTO messages (id, message_id, session_id, role, content_json, created_timestamp, tokens) VALUES
 (1,'msg_w1','20260708_1','user','[{"type":"text","text":"hello from the second machine"}]',1783548000,NULL),
 (2,'msg_w2','20260708_2','user','[{"type":"text","text":"another session on the second machine"}]',1783548600,NULL);

CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at TIMESTAMP);
INSERT INTO schema_version VALUES (14,'2026-07-08 22:00:00');
`
