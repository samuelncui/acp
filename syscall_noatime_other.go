//go:build !linux

package acp

// openNoAtime has no portable equivalent outside Linux; the flag is absent there.
const openNoAtime = 0
