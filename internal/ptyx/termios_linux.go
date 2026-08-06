//go:build linux

package ptyx

import "golang.org/x/sys/unix"

// getTermiosReq is the ioctl that reads a terminal's attributes. BSD and
// Linux spell it differently, so it lives in a per-platform file rather than
// behind a runtime check.
const getTermiosReq = unix.TCGETS
