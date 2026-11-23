package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"
	"testing"
)

func TestFinishThatReturnsItsCauseDoesNotInheritTheSourceSettlement(t *testing.T) {
	// A writer that returns or wraps its cause has not settled the target stream, even though the
	// cause carries the source's own ErrMemberSettled.
	for _, finish := range map[string]func(error) error{
		"returns cause": func(cause error) error { return cause },
		"wraps cause":   func(cause error) error { return fmt.Errorf("finish member: %w", cause) },
		"joins cause":   func(cause error) error { return errors.Join(errors.New("framing incomplete"), cause) },
	} {
		writer := &memberWriter{members: map[string][]byte{}}
		items := make([]Item, 3)
		for i, data := range []string{"first", "second", "third"} {
			target := writer.target(data)
			failure := error(nil)
			if i == 1 {
				failure = errors.Join(ErrMemberSettled, syscall.EIO)
				target = testContentTarget(func(context.Context, ContentMetadata) (ContentWriter, error) {
					return &testContentWriter{Writer: io.Discard, finish: finish}, nil
				})
			}
			items[i] = &testContentItem{SimpleJob: &SimpleJob{Path: fmt.Sprintf("member-%d", i), Dsts: []string{"out-" + data}},
				source: contentMember(data, failure), targets: []ContentTarget{target}}
		}
		onResults, _ := collectResults()
		err := runStream(context.Background(), onResults, items, SetFromDevice(LinearDevice(true)), SetToDevice(LinearDevice(true)),
			WithHashPolicy(HashRead))
		if err == nil || writer.members["third"] != nil {
			t.Fatalf("run error = %v, third member = %q; a Finish that only passes its cause on must stop", err, writer.members["third"])
		}
	}
}

func TestFinishSettlesWithItsOwnMarkerBesideTheCause(t *testing.T) {
	// A writer that settles wraps ErrMemberSettled itself; keeping the cause beside it is fine.
	writer := &memberWriter{members: map[string][]byte{}}
	items := make([]Item, 3)
	for i, data := range []string{"first", "second", "third"} {
		target := writer.target(data)
		failure := error(nil)
		if i == 1 {
			failure = errors.Join(ErrMemberSettled, syscall.EIO)
			target = testContentTarget(func(context.Context, ContentMetadata) (ContentWriter, error) {
				return &testContentWriter{Writer: io.Discard, finish: func(cause error) error {
					return fmt.Errorf("%w, padded: %w", ErrMemberSettled, cause)
				}}, nil
			})
		}
		items[i] = &testContentItem{SimpleJob: &SimpleJob{Path: fmt.Sprintf("member-%d", i), Dsts: []string{"out-" + data}},
			source: contentMember(data, failure), targets: []ContentTarget{target}}
	}
	onResults, _ := collectResults()
	if err := runStream(context.Background(), onResults, items, SetFromDevice(LinearDevice(true)), SetToDevice(LinearDevice(true)),
		WithHashPolicy(HashRead)); err != nil || string(writer.members["third"]) != "third" {
		t.Fatalf("run error = %v, third member = %q", err, writer.members["third"])
	}
}

// uncomparableError is an error value whose dynamic type cannot be compared with ==.
type uncomparableError struct{ parts []string }

func (e uncomparableError) Error() string { return fmt.Sprint(e.parts) }

// settledMatcher settles through an Is method, as errors.Is allows.
type settledMatcher struct{}

func (settledMatcher) Error() string        { return "settled by method" }
func (settledMatcher) Is(target error) bool { return target == ErrMemberSettled }

func TestSettledByWriterCountsOnlyMarkersOutsideTheCause(t *testing.T) {
	cause := error(&finishCause{err: errors.Join(ErrMemberSettled, syscall.EIO)})
	for name, test := range map[string]struct {
		err  error
		want bool
	}{
		"nil":                        {nil, false},
		"cause":                      {cause, false},
		"wrapped cause":              {fmt.Errorf("finish: %w", cause), false},
		"joined cause":               {errors.Join(errors.New("framing"), cause), false},
		"own marker":                 {fmt.Errorf("%w, padded", ErrMemberSettled), true},
		"own marker beside cause":    {errors.Join(fmt.Errorf("%w, padded", ErrMemberSettled), cause), true},
		"own marker through Is":      {settledMatcher{}, true},
		"uncomparable error":         {uncomparableError{parts: []string{"a"}}, false},
		"uncomparable beside marker": {errors.Join(uncomparableError{}, ErrMemberSettled), true},
	} {
		if got := settledByWriter(test.err, cause); got != test.want {
			t.Errorf("%s: settledByWriter = %t, want %t", name, got, test.want)
		}
	}
}
