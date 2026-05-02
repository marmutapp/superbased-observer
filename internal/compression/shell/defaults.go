package shell

import (
	"embed"
	"fmt"
	"io/fs"
)

//go:embed filters/defaults
var defaultsFS embed.FS

// LoadDefaults merges the embedded default filter specs into the engine.
// Safe to call multiple times; later calls replace same-key specs.
func (e *Engine) LoadDefaults() error {
	sub, err := fs.Sub(defaultsFS, "filters/defaults")
	if err != nil {
		return fmt.Errorf("shell.Engine.LoadDefaults: %w", err)
	}
	return e.LoadFS(sub)
}

// NewEngineWithDefaults returns an engine pre-populated with the embedded
// default filter specs.
func NewEngineWithDefaults() (*Engine, error) {
	e := NewEngine()
	if err := e.LoadDefaults(); err != nil {
		return nil, err
	}
	return e, nil
}
