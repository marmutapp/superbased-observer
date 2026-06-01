// Package failure categorizes command errors and produces stable
// command hashes for retry detection. The store layer calls into this
// package when ingesting run_command actions so that failure_context
// rows can be populated and retries correlated.
package failure
