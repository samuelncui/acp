package acp

import (
	"context"
	"errors"
	"syscall"
	"testing"
)

func TestDrainedMemberReportsItsSourceFailure(t *testing.T) {
	// A source failure while a refused member is drained is reported on that item, settled or not.
	for _, failure := range []error{syscall.EIO, errors.Join(ErrMemberSettled, syscall.EIO)} {
		t.Run(failure.Error(), func(t *testing.T) {
			item := refusedMemberItem("member", contentMember("member", failure))
			onResults, snapshot := collectResults()
			_ = runStream(context.Background(), onResults, []Item{item}, SetFromDevice(LinearDevice(true)),
				SetToDevice(LinearDevice(true)), WithHashPolicy(HashOff))
			if err := resultsByItem(t, snapshot())[item].Err; !errors.Is(err, ErrSourceRead) || !errors.Is(err, syscall.EIO) {
				t.Fatalf("drained member error = %v", err)
			}
		})
	}
}
