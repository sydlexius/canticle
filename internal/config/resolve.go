package config

// ResolveBool resolves a tri-state setting using the precedence
// CLI > per-library > global default. The first non-nil of cli then lib wins;
// otherwise globalDefault is returned. The built-in default folds into
// globalDefault at the call site.
func ResolveBool(cli, lib *bool, globalDefault bool) bool {
	if cli != nil {
		return *cli
	}
	if lib != nil {
		return *lib
	}
	return globalDefault
}
