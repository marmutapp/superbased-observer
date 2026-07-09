// Package sqlitedsn builds the path component of SQLite "file:" DSN
// URIs safely. It is the one owner of DSN path escaping (security
// ledger L8): every adapter, backfill, and first-party db.Open that
// constructs a "file:" URI routes its filesystem path through Escape
// so that characters the SQLite URI parser treats specially cannot
// smuggle query parameters or truncate the path.
package sqlitedsn
