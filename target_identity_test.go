package acp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
)

type delayedPublicationOutput struct {
	transferOutput
	gate <-chan struct{}
}

func (o *delayedPublicationOutput) Commit(overwrite bool) error {
	// Hold final replacement while another file is free to complete its own data and metadata.
	<-o.gate
	return o.transferOutput.Commit(overwrite)
}

func TestRelativeAndAbsoluteTargetsKeepSerialWinner(t *testing.T) {
	// Both device modes formerly finished each file before admitting the next writer.
	for _, config := range []struct {
		name   string
		linear bool
		linked bool
	}{
		{"random", false, false},
		{"linear", true, false},
		{"random-linked-cwd", false, true},
		{"linear-linked-cwd", true, true},
	} {
		t.Run(config.name, func(t *testing.T) {
			for _, overwrite := range []bool{false, true} {
				t.Run(map[bool]string{false: "exclusive", true: "overwrite"}[overwrite], func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						// Index both names of one stable destination before either output is created.
						c := newTestStream(t, Overwrite(overwrite), WithHashPolicy(HashOff),
							SetFromDevice(DeviceThreads(1)), SetToDevice(DeviceThreads(1), LinearDevice(config.linear)))
						root := t.TempDir()
						cwd := root
						if config.linked {
							cwd = filepath.Join(root, "alias")
							if err := os.Symlink(root, cwd); err != nil {
								t.Skipf("symlinks unsupported: %v", err)
							}
						}
						t.Chdir(cwd)
						absolute := filepath.Join(root, "target")
						if overwrite {
							writeSourceFile(t, root, "target", []byte("original"))
						}
						names := []string{"target", absolute}
						prepared := make(chan *writeJob, 2)
						for i, name := range names {
							content := []string{"first", "second"}[i]
							source := writeSourceFile(t, root, content, []byte(content))
							job := c.buildJob(&SimpleJob{Path: source, Dsts: []string{name}}, uint64(i))
							if job.itemError != nil || len(job.outputs) != 1 {
								t.Fatalf("index outcome: %+v", job.result())
							}
							prepared <- c.prepareItem(job)
						}
						close(prepared)

						// Delay the first native replacement so an unrelated publication could overtake it.
						gate := make(chan struct{})
						native := c.fs()
						c.filesystem = pipelineFilesystem{
							transferFilesystem: native,
							create: func(job *writeJob, spec targetSpec) (transferOutput, error) {
								// Retain the real filesystem lifecycle while holding only the first publication.
								out, err := native.Create(job, spec)
								if err != nil {
									return nil, err
								}
								if job.order != 0 {
									return out, nil
								}
								return &delayedPublicationOutput{transferOutput: out, gate: gate}, nil
							},
						}
						out := c.copy(context.Background(), prepared)
						synctest.Wait()
						close(gate)

						// Keep the serial winner and each caller's original name in the actual outcomes.
						count := 0
						for job := range out {
							count++
							result := job.result()
							if result.Err != nil || len(result.Targets) != 1 || result.Targets[0].Path != names[job.order] {
								t.Fatalf("result lost its logical target: %+v", result)
							}
							err := result.Targets[0].Err
							if !overwrite && job.order == 1 {
								if !errors.Is(err, os.ErrExist) {
									t.Errorf("later exclusive output error=%v, want existence failure", err)
								}
								continue
							}
							if err != nil {
								t.Errorf("order=%d failed: %v", job.order, err)
							}
						}
						want := "first"
						if overwrite {
							want = "second"
						}
						data, err := os.ReadFile(absolute)
						if err != nil || string(data) != want || count != 2 {
							t.Fatalf("final target=%q, want %q; results=%d: %v", data, want, count, err)
						}
					})
				})
			}
		})
	}
}

func TestTargetParentTraversalFromSymlinkedWorkingDirectory(t *testing.T) {
	// Both ordinary targets and existing target links follow the physical parent of cwd.
	for _, name := range []string{"../target", "../link"} {
		t.Run(name, func(t *testing.T) {
			// The logical parent holds an unrelated file that replacement must never touch.
			root := t.TempDir()
			parent := filepath.Join(root, "tree")
			child := filepath.Join(parent, "child")
			if err := os.MkdirAll(child, 0o755); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(root, "alias")
			if err := os.Symlink(child, alias); err != nil {
				t.Skipf("symlinks unsupported: %v", err)
			}
			t.Chdir(alias)
			source := writeSourceFile(t, root, "source", []byte("new content"))
			target := writeSourceFile(t, parent, "target", []byte("original target"))
			decoy := writeSourceFile(t, root, "target", []byte("unrelated content"))
			if err := os.Symlink("target", filepath.Join(parent, "link")); err != nil {
				t.Fatal(err)
			}

			// Native copying must use the resolved identity while reporting the caller's relative name.
			item := newFixtureItem(source, name)
			if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, Overwrite(true)); err != nil {
				t.Fatal(err)
			}
			result, err := item.terminal(t)
			if err != nil || len(result.Targets) != 1 || result.Targets[0].Path != name || result.Targets[0].Err != nil {
				t.Fatalf("copy result: %+v / %v", result, err)
			}
			for path, want := range map[string]string{target: "new content", decoy: "unrelated content"} {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != want {
					t.Errorf("path=%q content=%q, want %q: %v", path, data, want, err)
				}
			}
		})
	}
}
