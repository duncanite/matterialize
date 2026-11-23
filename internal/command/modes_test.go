package command_test

import "runtime"

// runtimeHasModes says whether files have the modes this module sets:
// Windows has none of them.
func runtimeHasModes() bool { return runtime.GOOS != "windows" }
