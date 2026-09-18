//go:build linux

package acp

import "syscall"

// openNoAtime keeps source reads out of each file's access time on Linux.
const openNoAtime = syscall.O_NOATIME
