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

func TestNativeTargetAliasesKeepSerialWinner(t *testing.T) {
	// Cover every device configuration that promises ordered completion.
	for _, config := range []struct {
		name                       string
		sourceLinear, targetLinear bool
		targetThreads              int
	}{
		{"linear-target", false, true, 1},
		{"single-writer", false, false, 1},
		{"linear-source", true, false, 3},
	} {
		for _, alias := range []struct {
			name  string
			names [2]string
		}{
			{"case", [2]string{"target", "TARGET"}},
			{"unicode-decomposition", [2]string{"caf\u00e9", "cafe\u0301"}},
		} {
			t.Run(config.name+"/"+alias.name, func(t *testing.T) {
				// Native name lookup determines whether this fixture has one winner or two distinct outputs.
				root := t.TempDir()
				probe := writeSourceFile(t, root, alias.names[0], []byte("probe"))
				first, err := os.Stat(probe)
				if err != nil {
					t.Fatal(err)
				}
				second, err := os.Stat(filepath.Join(root, alias.names[1]))
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				nativeAlias := err == nil && os.SameFile(first, second)
				t.Logf("native alias probe: %q and %q alias=%t", alias.names[0], alias.names[1], nativeAlias)

				// Existing exclusive targets are the indexing control; the other cases exercise publication.
				for _, test := range []struct {
					name      string
					exists    bool
					overwrite bool
				}{
					{name: "missing-exclusive"},
					{name: "missing-overwrite", overwrite: true},
					{name: "existing-exclusive", exists: true},
					{name: "existing-overwrite", exists: true, overwrite: true},
				} {
					t.Run(test.name, func(t *testing.T) {
						synctest.Test(t, func(t *testing.T) {
							// Index stable inputs and both native target aliases before any copy changes the destination.
							c := newTestStream(t, Overwrite(test.overwrite), WithHashPolicy(HashOff),
								SetFromDevice(LinearDevice(config.sourceLinear)),
								SetToDevice(LinearDevice(config.targetLinear), DeviceThreads(config.targetThreads)))
							dir := filepath.Join(root, test.name)
							names := []string{filepath.Join(dir, alias.names[0]), filepath.Join(dir, alias.names[1]),
								filepath.Join(dir, "independent")}
							if test.exists {
								writeSourceFile(t, dir, alias.names[0], []byte("original"))
							}
							prepared := make(chan *writeJob, len(names))
							for i, name := range names {
								content := []string{"first", "second", "independent"}[i]
								source := writeSourceFile(t, filepath.Join(dir, "sources"), content, []byte(content))
								job := c.buildJob(&SimpleJob{Path: source, Dsts: []string{name}}, uint64(i))
								if job.itemError != nil {
									t.Fatalf("index outcome: %+v", job.result())
								}
								wantOutputs := 1
								if i < 2 && test.exists && !test.overwrite && (i == 0 || nativeAlias) {
									wantOutputs = 0
								}
								if len(job.outputs) != wantOutputs {
									t.Fatalf("index order=%d: %+v", i, job.result())
								}
								wj := c.prepareItem(job)
								t.Cleanup(func() { _ = wj.finishSource() })
								prepared <- wj
							}
							close(prepared)

							// Hold only the first Commit; native writes, metadata and renames remain real.
							gate := make(chan struct{})
							release := onceRelease(gate)
							defer release()
							native := c.fs()
							c.filesystem = pipelineFilesystem{
								transferFilesystem: native,
								create: func(job *writeJob, spec targetSpec) (transferOutput, error) {
									// Retain native output ownership and delay only the first item's replacement.
									out, err := native.Create(job, spec)
									if err != nil || job.order != 0 {
										return out, err
									}
									return &delayedPublicationOutput{transferOutput: out, gate: gate}, nil
								},
							}
							out := c.copy(context.Background(), prepared)
							synctest.Wait()

							// A fix must hold the alias while allowing the unrelated file in the same directory to publish.
							if data, err := os.ReadFile(names[2]); err != nil || string(data) != "independent" {
								t.Errorf("independent output blocked by alias completion: %q / %v", data, err)
							}
							data, err := os.ReadFile(names[0])
							t.Logf("before first Commit: target=%q error=%v", data, err)
							if test.exists {
								if err != nil || string(data) != "original" {
									t.Errorf("alias published before first Commit: %q / %v", data, err)
								}
							} else if !errors.Is(err, os.ErrNotExist) {
								t.Errorf("missing alias published before first Commit: %q / %v", data, err)
							}
							release()

							// Drain all real outcomes and retain the first exclusive or last overwrite winner.
							count := 0
							for job := range out {
								count++
								result := job.result()
								if result.Err != nil || len(result.Targets) != 1 || result.Targets[0].Path != names[job.order] {
									t.Errorf("unexpected result: %+v", result)
									continue
								}
								targetErr := result.Targets[0].Err
								t.Logf("order=%d target error=%v", job.order, targetErr)
								refused := test.exists && job.order == 0 || nativeAlias && job.order == 1
								if !test.overwrite && refused {
									if !errors.Is(targetErr, os.ErrExist) {
										t.Errorf("order=%d: error=%v, want existence failure", job.order, targetErr)
									}
								} else if targetErr != nil {
									t.Errorf("order=%d failed: %v", job.order, targetErr)
								}
							}
							want := "first"
							if test.overwrite && nativeAlias {
								want = "second"
							} else if test.exists && !test.overwrite {
								want = "original"
							}
							data, err = os.ReadFile(names[0])
							t.Logf("final target=%q, want %q; results=%d", data, want, count)
							if err != nil || string(data) != want || count != len(names) {
								t.Errorf("wrong serial winner: %q / %v, want %q; results=%d", data, err, want, count)
							}
							if !nativeAlias {
								second, err := os.ReadFile(names[1])
								if err != nil || string(second) != "second" {
									t.Errorf("distinct native target lost its content: %q / %v", second, err)
								}
							}
						})
					})
				}
			})
		}
	}
}

func BenchmarkPublicationOrder(b *testing.B) {
	// Measure the per-file completion dependency with ordinary and Unicode target names.
	for _, name := range []string{"/volume/archive/file-000001.bin", "/volume/Archive/File-000001.bin", "/volume/caf\u00e9/file-000001.bin"} {
		b.Run(name, func(b *testing.B) {
			order := newPublicationOrder()
			b.ReportAllocs()
			for b.Loop() {
				turn := order.reserve(name)
				turn.release()
			}
		})
	}
}
