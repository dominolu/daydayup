// Package testsupport wires the built-in platforms' declarations into test
// binaries. The gateway installs them at boot (they live in the adapter and
// core provider init functions); tests exercise config, accounts and gateway
// packages directly, so importing this file from a test keeps the same
// platform vocabulary the binary runs with.
package testsupport

import (
	_ "safuno/internal/provider"
	_ "safuno/internal/provider/workbuddy"
)
