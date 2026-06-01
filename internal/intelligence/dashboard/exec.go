package dashboard

import "os"

// osExecutableImpl is the default implementation of osExecutable —
// kept in its own file so the test file can swap the var without
// dragging the os dependency into a package that doesn't otherwise
// need it.
func osExecutableImpl() (string, error) {
	return os.Executable()
}
