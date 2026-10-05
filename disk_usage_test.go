package acp

import (
	"errors"
	"syscall"
	"testing"
)

// TestMappingErrorIdentities pins the identity of every mapped target failure. An I/O error
// describes one target, not a device that turned read-only, so it must not abort the target.
func TestMappingErrorIdentities(t *testing.T) {
	tests := []struct {
		name      string
		from      error
		want      error
		wantAbort bool
	}{
		{name: "no space", from: syscall.ENOSPC, want: ErrTargetNoSpace, wantAbort: true},
		{name: "read-only device", from: syscall.EROFS, want: ErrTargetDropToReadonly, wantAbort: true},
		{name: "io error", from: syscall.EIO, want: ErrTargetIO, wantAbort: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mapped := mappingError(tt.from)
			if !errors.Is(mapped, tt.want) {
				t.Fatalf("mappingError(%v) = %v, want %v", tt.from, mapped, tt.want)
			}
			if got := checkErrorAbort(mapped); got != tt.wantAbort {
				t.Fatalf("checkErrorAbort(%v) = %t, want %t", mapped, got, tt.wantAbort)
			}
		})
	}

	// A target I/O error must never be classified as a device that dropped to read-only.
	if errors.Is(mappingError(syscall.EIO), ErrTargetDropToReadonly) {
		t.Fatal("an I/O error is still classified as a read-only device")
	}
	if err := mappingError(nil); err != nil {
		t.Fatalf("mappingError(nil) = %v, want nil", err)
	}
}
