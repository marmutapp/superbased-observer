package toolresolve

// commonNativeProbeDirs is the shared, HOME-relative table of directories the
// resolver scans for a tool's binary when it is not first on PATH. These are
// the npm/volta/pnpm/bun/nvm-style install prefixes common ACROSS tools; a
// per-tool BinaryResolveSpec carries only the EXTRAS the common table misses
// (integration.BinaryResolveSpec doc comment). Entries are Unix-flavored — a
// Windows native daemon still walks them, they simply do not exist there and
// stat misses harmlessly. An entry containing a single "*" segment (e.g. the
// nvm node-version dir) is glob-expanded by the resolver against the home.
var commonNativeProbeDirs = []string{
	".local/bin",
	"bin",
	".npm-global/bin",
	".volta/bin",
	".bun/bin",
	".local/share/pnpm",
	".nvm/versions/node/*/bin",
	".hermes/node/bin",
	".opencode/bin",
}

// commonForeignWindowsDirs is the shared, HOME-relative table of directories
// the resolver scans under each Windows user home reached over /mnt from a WSL
// daemon (foreign probe). These are the standard npm / packaged-app / scoop
// install prefixes a Windows install lays down. Scanned ONLY when the tool has
// grounded Windows binary spellings (BinaryNames.Windows non-empty); a hit is
// classified Foreign — evidence the tool is installed on Windows but not
// natively, never a launchable candidate.
var commonForeignWindowsDirs = []string{
	"AppData/Roaming/npm",
	"AppData/Local/Programs",
	"scoop/shims",
	".local/bin",
}
