// Package watcher runs the fsnotify-based file watcher daemon. It dispatches
// file change events to the appropriate adapter and pipes parsed ToolEvents
// through a buffered channel to the storage goroutine. See spec §18.
package watcher
