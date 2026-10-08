package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLinearCopyChunkBoundaries(t *testing.T) {
	// Exercise complete files around EOF with both native source readers.
	for _, mode := range []ReadMode{ReadBuffered, ReadMapped} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			// Reusing one destination makes the final content depend on linear request order.
			root := t.TempDir()
			target := filepath.Join(root, "target")
			contents := [][]byte{nil, bytes.Repeat([]byte("x"), batchSize), bytes.Repeat([]byte("t"), batchSize+17)}
			items := make([]Item, 0, len(contents))
			for index, content := range contents {
				source := writeSourceFile(t, root, fmt.Sprintf("source-%d", index), content)
				items = append(items, &SimpleJob{Path: source, Dsts: []string{target}})
			}

			// Wait includes consumer closure, target finalization and complete result delivery.
			callback, batches := collectResults()
			if err := runStream(context.Background(), callback, items, Overwrite(true), WithHashPolicy(HashReadRefresh),
				SetFromDevice(WithReadMode(mode)), SetToDevice(LinearDevice(true))); err != nil {
				t.Fatal(err)
			}

			// Every accepted item reports its complete size and hash exactly once.
			seen := make(map[Item]bool)
			for _, batch := range batches() {
				for _, result := range batch {
					if seen[result.Job] {
						t.Fatalf("duplicate result for %v", result.Job)
					}
					seen[result.Job] = true
					index := -1
					for i, item := range items {
						if item == result.Job {
							index = i
							break
						}
					}
					if index < 0 {
						t.Fatalf("unexpected result for %v", result.Job)
					}
					want := sha256.Sum256(contents[index])
					if result.Err != nil || result.Size != int64(len(contents[index])) || !bytes.Equal(result.SHA256, want[:]) {
						t.Fatalf("incomplete result: %+v", result)
					}
					if len(result.Targets) != 1 || result.Targets[0].Err != nil {
						t.Fatalf("target result: %+v", result.Targets)
					}
				}
			}
			if len(seen) != len(items) {
				t.Fatalf("results=%d want=%d", len(seen), len(items))
			}

			// The last submitted file owns the destination after all target writers have closed.
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(got, contents[len(contents)-1]) {
				t.Fatalf("final ordered target: bytes=%d err=%v", len(got), err)
			}
		})
	}
}
