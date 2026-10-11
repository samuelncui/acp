package acp

import (
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/samuelncui/acp/internal/fileio"
)

type serialPublicationOutput struct {
	*pipelineOutput
	sync    func() error
	publish func(bool) error
}

func (o *serialPublicationOutput) Sync() error                 { return o.sync() }
func (o *serialPublicationOutput) Commit(overwrite bool) error { return o.publish(overwrite) }

func TestSerialRandomTargetPreservesConflictingPublicationOrder(t *testing.T) {
	// Exercise each configuration that previously completed conflicting copies in a fixed order.
	for _, config := range []struct {
		name          string
		sourceLinear  bool
		targetThreads int
	}{
		{"linear-source-serial-target", true, 1},
		{"random-source-serial-target", false, 1},
		{"linear-source-parallel-target", true, 3},
	} {
		t.Run(config.name, func(t *testing.T) {
			for _, overwrite := range []bool{false, true} {
				t.Run(map[bool]string{false: "exclusive", true: "overwrite"}[overwrite], func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						// Delay the first completion while later data and a distinct publication advance.
						c := newTestStream(t, WithHashPolicy(HashOff), Overwrite(overwrite),
							SetFromDevice(LinearDevice(config.sourceLinear), DeviceThreads(1)), SetToDevice(DeviceThreads(config.targetThreads)))
						release, independent := make(chan struct{}), make(chan struct{})
						laterCreated := make(chan struct{})
						var lock sync.Mutex
						published := make(map[string]uint64)
						c.filesystem = pipelineFilesystem{create: func(job *writeJob, spec targetSpec) (transferOutput, error) {
							if config.targetThreads > 1 {
								if job.order == 0 {
									<-laterCreated
								}
								if job.order == 1 {
									close(laterCreated)
								}
							}
							return &serialPublicationOutput{
								pipelineOutput: &pipelineOutput{write: func(data []byte) (int, error) { return len(data), nil }},
								sync: func() error {
									if job.order == 0 {
										<-release
									}
									return nil
								},
								publish: func(overwrite bool) error {
									lock.Lock()
									defer lock.Unlock()
									if _, exists := published[spec.path]; exists && !overwrite {
										return os.ErrExist
									}
									published[spec.path] = job.order
									if job.order == 2 {
										close(independent)
									}
									return nil
								},
							}, nil
						}}
						prepared := make(chan *writeJob, 3)
						for i, target := range []string{"same-target", "same-target", "independent-target"} {
							prepared <- preparedFixture(c, uint64(i), 4096, target, &generatedReader{remaining: 4096})
						}
						close(prepared)
						out := c.copy(context.Background(), prepared)
						<-independent
						synctest.Wait()
						lock.Lock()
						_, premature := published["same-target"]
						lock.Unlock()
						if premature {
							t.Error("conflicting output published before the prior completion")
						}

						// The old single-writer winner remains first for exclusive creation and last for overwrite.
						close(release)
						failures, count := 0, 0
						for job := range out {
							count++
							if err := job.result().Targets[0].Err; err != nil {
								failures++
								if !errors.Is(err, os.ErrExist) || job.order != 1 {
									t.Errorf("unexpected target error: order=%d error=%v", job.order, err)
								}
							}
						}
						want, wantFailures := uint64(0), 1
						if overwrite {
							want, wantFailures = 1, 0
						}
						if count != 3 || failures != wantFailures || published["same-target"] != want {
							t.Fatalf("publication outcomes: count=%d failures=%d winners=%v", count, failures, published)
						}
					})
				})
			}
		})
	}
}

func TestPublicationReleasesFailedAliasesAfterSourceCache(t *testing.T) {
	// Case and decomposition aliases retain their failure chain around a rewrite output.
	synctest.Test(t, func(t *testing.T) {
		c := newTestStream(t, WithHashPolicy(HashOff), SetToDevice(DeviceThreads(1)))
		job := preparedFixture(c, 0, 4096, "same-target", &generatedReader{remaining: 4096})
		job.targets = []string{"first", "rewrite", "last"}
		job.outputs = []targetSpec{
			{name: "first", path: "CAF\u00c9", device: "fixture"},
			{name: "rewrite", path: "cafe\u0301", device: "fixture", output: &fileio.Output{}},
			{name: "last", path: "Caf\u00e9", device: "fixture"},
		}
		committed := false
		c.filesystem = pipelineFilesystem{create: func(_ *writeJob, spec targetSpec) (transferOutput, error) {
			if spec.name != "rewrite" {
				return nil, syscall.EIO
			}
			return &pipelineOutput{
				write:  func(data []byte) (int, error) { return len(data), nil },
				commit: func() error { committed = true; return nil },
			}, nil
		}}
		prepared := make(chan *writeJob, 1)
		prepared <- job
		close(prepared)

		// Cleanup must finish the source cache before releasing failed tickets in original output order.
		count := 0
		for result := range c.copy(context.Background(), prepared) {
			count++
			targets := result.result().Targets
			if len(targets) != 3 || targets[1].Err != nil ||
				!errors.Is(targets[0].Err, ErrTargetIO) || !errors.Is(targets[2].Err, ErrTargetIO) {
				t.Errorf("alias outcomes: %+v", targets)
			}
		}
		if count != 1 || !committed {
			t.Fatalf("unfinished alias chain: count=%d committed=%t", count, committed)
		}
	})
}

func TestDirectAliasesWithinItemWaitOnlyForPrecedingItem(t *testing.T) {
	for _, test := range []struct {
		name        string
		firstDirect bool
		failFirst   bool
	}{
		{name: "ordinary-first"},
		{name: "direct-first", firstDirect: true},
		{name: "ordinary-create-failure", failFirst: true},
		{name: "direct-create-failure", firstDirect: true, failFirst: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Keep an earlier device lifetime outstanding while the next item prepares aliases.
				c := newTestStream(t, WithHashPolicy(HashOff), Overwrite(true),
					SetFromDevice(LinearDevice(true)), SetToDevice(DeviceThreads(3)))
				gate, holding := make(chan struct{}), make(chan struct{})
				release := onceRelease(gate)
				defer release()
				created, committed := make(chan string, 3), make(chan string, 3)
				c.filesystem = pipelineFilesystem{create: func(job *writeJob, spec targetSpec) (transferOutput, error) {
					// Creation failures must release their ticket without stranding the direct sibling.
					created <- spec.name
					if spec.name == "first" && test.failFirst {
						return nil, syscall.EIO
					}
					written := 0
					return &serialPublicationOutput{
						pipelineOutput: &pipelineOutput{write: func(data []byte) (int, error) {
							written += len(data)
							return len(data), nil
						}},
						sync: func() error {
							if job.order == 0 {
								close(holding)
								<-gate
							}
							return nil
						},
						publish: func(bool) error {
							if written != 4096 {
								t.Errorf("target %q published %d bytes, want 4096", spec.name, written)
							}
							committed <- spec.name
							return nil
						},
					}, nil
				}}
				preceding := preparedFixture(c, 0, 4096, "preceding", &generatedReader{remaining: 4096})
				preceding.outputs[0].path, preceding.outputs[0].direct = "CAF\u00c9", true
				siblings := preparedFixture(c, 1, 4096, "first", &generatedReader{remaining: 4096})
				siblings.targets = []string{"first", "second"}
				siblings.outputs = []targetSpec{
					{name: "first", path: "cafe\u0301", device: "fixture", direct: test.firstDirect},
					{name: "second", path: "Caf\u00e9", device: "fixture", direct: true},
				}
				prepared := make(chan *writeJob, 2)
				prepared <- preceding
				prepared <- siblings
				close(prepared)
				out := c.copy(context.Background(), prepared)
				synctest.Wait()

				// Ordinary creation can overlap, but neither direct sibling may open ahead of the predecessor.
				select {
				case <-holding:
				default:
					t.Error("preceding output did not reach its held completion")
				}
				creates := make(map[string]int)
				for len(created) > 0 {
					creates[<-created]++
				}
				wantFirst := 1
				if test.firstDirect {
					wantFirst = 0
				}
				if creates["preceding"] != 1 || creates["first"] != wantFirst || creates["second"] != 0 || len(committed) != 0 {
					t.Errorf("premature direct creation or publication: creates=%v commits=%d", creates, len(committed))
				}

				// Once the prior item settles, both siblings must finish without waiting on their own item.
				release()
				synctest.Wait()
				want := []string{"preceding", "first", "second"}
				if test.failFirst {
					want = []string{"preceding", "second"}
				}
				if len(committed) != len(want) {
					t.Errorf("siblings stalled after predecessor release: commits=%d, want %d", len(committed), len(want))
					c.stopHard()
				}
				count := 0
				for job := range out {
					count++
					result := job.result()
					if result.Err != nil || len(result.Targets) != len(job.targets) {
						t.Errorf("unexpected item outcome: %+v", result)
					}
					for _, target := range result.Targets {
						if target.Path == "first" && test.failFirst {
							if !errors.Is(target.Err, ErrTargetIO) {
								t.Errorf("first creation error=%v, want I/O failure", target.Err)
							}
							continue
						}
						if target.Err != nil {
							t.Errorf("target %q failed: %v", target.Path, target.Err)
						}
					}
				}
				close(created)
				for name := range created {
					creates[name]++
				}
				close(committed)
				var publications []string
				for name := range committed {
					publications = append(publications, name)
				}
				if count != 2 || creates["preceding"] != 1 || creates["first"] != 1 || creates["second"] != 1 ||
					!slices.Equal(publications, want) {
					t.Errorf("unfinished aliases: results=%d creates=%v publications=%v, want %v", count, creates, publications, want)
				}
			})
		})
	}
}

func TestLinearSourceKeepsConflictingDirectOutputsExclusive(t *testing.T) {
	// Raw devices may reject a second Open until the prior Close; Commit cannot hide their writes.
	synctest.Test(t, func(t *testing.T) {
		c := newTestStream(t, WithHashPolicy(HashOff), Overwrite(true),
			SetFromDevice(LinearDevice(true)), SetToDevice(DeviceThreads(3)))
		release, independent := make(chan struct{}), make(chan struct{})
		var lock sync.Mutex
		opened := make(map[string]bool)
		c.filesystem = pipelineFilesystem{create: func(job *writeJob, spec targetSpec) (transferOutput, error) {
			lock.Lock()
			defer lock.Unlock()
			if opened[spec.path] {
				return nil, syscall.EBUSY
			}
			opened[spec.path] = true
			return &serialPublicationOutput{
				pipelineOutput: &pipelineOutput{write: func(data []byte) (int, error) { return len(data), nil }},
				sync: func() error {
					if job.order == 0 {
						<-release
					}
					return nil
				},
				publish: func(bool) error {
					lock.Lock()
					defer lock.Unlock()
					opened[spec.path] = false
					if job.order == 2 {
						close(independent)
					}
					return nil
				},
			}, nil
		}}
		prepared := make(chan *writeJob, 3)
		for i, target := range []string{"raw-device", "raw-device", "independent-device"} {
			job := preparedFixture(c, uint64(i), 4096, target, &generatedReader{remaining: 4096})
			job.outputs[0].direct = true
			prepared <- job
		}
		close(prepared)
		out := c.copy(context.Background(), prepared)
		<-independent
		synctest.Wait()

		// A separate device finishes while both same-device copies succeed in their original order.
		close(release)
		count := 0
		for job := range out {
			count++
			if err := job.result().Targets[0].Err; err != nil {
				t.Errorf("raw device copy failed: order=%d error=%v", job.order, err)
			}
		}
		if count != 3 || opened["raw-device"] || opened["independent-device"] {
			t.Fatalf("raw device outcomes: count=%d open=%v", count, opened)
		}
	})
}

func TestDirectOutputObservesNoSpaceAfterPriorClose(t *testing.T) {
	// A later direct target waits for the first device handle, whose Close discovers capacity exhaustion.
	synctest.Test(t, func(t *testing.T) {
		c := newTestStream(t, WithHashPolicy(HashOff), SetToDevice(LinearDevice(true)))
		closing, release := make(chan struct{}), make(chan struct{})
		creates := 0
		c.filesystem = pipelineFilesystem{create: func(job *writeJob, _ targetSpec) (transferOutput, error) {
			creates++
			return &pipelineOutput{
				write: func(data []byte) (int, error) { return len(data), nil },
				close: func() error {
					if job.order == 0 {
						close(closing)
						<-release
						return syscall.ENOSPC
					}
					return nil
				},
			}, nil
		}}
		prepared := make(chan *writeJob, 2)
		for i := range 2 {
			job := preparedFixture(c, uint64(i), 4096, "raw-device", &generatedReader{remaining: 4096})
			job.outputs[0].direct = true
			prepared <- job
		}
		close(prepared)
		out := c.copy(context.Background(), prepared)
		<-closing
		synctest.Wait()
		close(release)

		// The authoritative Close error stops the waiting output before another Create.
		count := 0
		for job := range out {
			count++
			if err := job.result().Targets[0].Err; !errors.Is(err, ErrTargetNoSpace) {
				t.Errorf("lost no-space outcome: order=%d error=%v", job.order, err)
			}
		}
		if count != 2 || creates != 1 {
			t.Fatalf("exhausted direct target admitted again: count=%d creates=%d", count, creates)
		}
	})
}
