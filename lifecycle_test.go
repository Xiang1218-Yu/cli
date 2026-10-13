package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// lifecycleChain builds root -> sub -> leaf commands that record every hook
// and every cleanup they run into log, registering one cleanup per hook.
func lifecycleChain(log *[]string, enabled bool, leafAction func(context.Context, *Command) error) *Command {
	mark := func(name string) { *log = append(*log, name) }
	reg := func(ctx context.Context, name string) {
		RegisterCleanup(ctx, name, func(context.Context) error {
			*log = append(*log, "cleanup:"+name)
			return nil
		})
	}

	leaf := &Command{
		Name: "leaf",
		Before: func(ctx context.Context, c *Command) (context.Context, error) {
			mark("before:leaf")
			reg(ctx, "leaf-before")
			return ctx, nil
		},
		Action: func(ctx context.Context, c *Command) error {
			mark("action:leaf")
			reg(ctx, "leaf-action")
			if leafAction != nil {
				return leafAction(ctx, c)
			}
			return nil
		},
		After: func(context.Context, *Command) error {
			mark("after:leaf")
			return nil
		},
	}

	sub := &Command{
		Name: "sub",
		Before: func(ctx context.Context, c *Command) (context.Context, error) {
			mark("before:sub")
			reg(ctx, "sub-before")
			return ctx, nil
		},
		After: func(context.Context, *Command) error {
			mark("after:sub")
			return nil
		},
		Commands: []*Command{leaf},
	}

	return &Command{
		Name:            "app",
		EnableLifecycle: enabled,
		Writer:          discardWriter{},
		ErrWriter:       discardWriter{},
		Before: func(ctx context.Context, c *Command) (context.Context, error) {
			mark("before:root")
			reg(ctx, "root-before")
			return ctx, nil
		},
		After: func(context.Context, *Command) error {
			mark("after:root")
			return nil
		},
		Commands: []*Command{sub},
	}
}

func lifecycleErr(t *testing.T, err error) *LifecycleError {
	t.Helper()
	var le *LifecycleError
	require.ErrorAs(t, err, &le)
	return le
}

func TestLifecycle_DefaultOffKeepsLegacySemantics(t *testing.T) {
	var log []string
	cmd := lifecycleChain(&log, false, nil)

	// RegisterCleanup is a no-op without lifecycle orchestration.
	require.False(t, RegisterCleanup(context.Background(), "x", func(context.Context) error { return nil }))

	require.NoError(t, cmd.Run(context.Background(), []string{"app", "sub", "leaf"}))

	require.Equal(t, []string{
		"before:root", "before:sub", "before:leaf",
		"action:leaf",
		"after:leaf", "after:sub", "after:root",
	}, log)
}

func TestLifecycle_OrderedPhasesAndReverseCleanup(t *testing.T) {
	var log []string
	var events []string
	cmd := lifecycleChain(&log, true, nil)
	cmd.LifecycleObserver = func(ev LifecycleEvent) {
		if ev.Type == LifecycleStageBegin {
			events = append(events, "begin:"+ev.Step.String())
		}
		if ev.Type == LifecycleEnd {
			events = append(events, "end:"+string(ev.Entry))
		}
	}

	require.NoError(t, cmd.Run(context.Background(), []string{"app", "sub", "leaf"}))

	require.Equal(t, []string{
		"before:root", "before:sub", "before:leaf",
		"action:leaf",
		// finalize runs leaf -> root, strictly after execute
		"after:leaf", "after:sub", "after:root",
		// cleanup is one phase, reverse registration order
		"cleanup:leaf-action", "cleanup:leaf-before",
		"cleanup:sub-before", "cleanup:root-before",
	}, log)

	// Observer sees the same ordering and exactly one invocation end.
	require.Equal(t, []string{
		"begin:prepare app",
		"begin:prepare app sub",
		"begin:prepare app sub leaf",
		"begin:execute app sub leaf",
		"begin:finalize app sub leaf",
		"begin:finalize app sub",
		"begin:finalize app",
		"begin:cleanup \"leaf-action\"",
		"begin:cleanup \"leaf-before\"",
		"begin:cleanup \"sub-before\"",
		"begin:cleanup \"root-before\"",
		"end:run",
	}, events)
}

func TestLifecycle_CleanupOnceAcrossRepeatedRuns(t *testing.T) {
	var mu sync.Mutex
	var runs []string

	root := &Command{
		Name:            "app",
		EnableLifecycle: true,
		Writer:          discardWriter{},
		ErrWriter:       discardWriter{},
		Action: func(ctx context.Context, c *Command) error {
			record := func(name string) {
				require.True(t, RegisterCleanup(ctx, name, func(context.Context) error {
					mu.Lock()
					runs = append(runs, name)
					mu.Unlock()
					return nil
				}))
			}
			record("a")
			record("b")
			record("c")
			return nil
		},
	}

	for range 3 {
		require.NoError(t, root.Run(context.Background(), []string{"app"}))
	}

	// Every invocation: c, b, a. No cleanup is skipped or duplicated, and no
	// state leaks between invocations of the same Command.
	require.Equal(t, []string{
		"c", "b", "a",
		"c", "b", "a",
		"c", "b", "a",
	}, runs)
}

var errActionSentinel = errors.New("action blew up")

func TestLifecycle_ActionErrorKeepsPhaseAndCleanupRelationship(t *testing.T) {
	var log []string
	cleanupErr := errors.New("cleanup failed")
	cmd := lifecycleChain(&log, true, func(ctx context.Context, c *Command) error {
		require.True(t, RegisterCleanup(ctx, "failing", func(context.Context) error {
			return cleanupErr
		}))
		return errActionSentinel
	})

	err := cmd.Run(context.Background(), []string{"app", "sub", "leaf"})
	le := lifecycleErr(t, err)

	// Primary error remains directly discoverable.
	require.ErrorIs(t, err, errActionSentinel)
	require.Same(t, errActionSentinel, le.Cause())
	require.Equal(t, LifecycleEntryRun, le.Entry())

	// All teardown still ran despite the action failure...
	require.Equal(t, []string{
		"before:root", "before:sub", "before:leaf", "action:leaf",
		"after:leaf", "after:sub", "after:root",
		"cleanup:leaf-action", "cleanup:leaf-before",
		"cleanup:sub-before", "cleanup:root-before",
	}, log)
	// ...and the failing cleanup error is preserved separately and never
	// replaces the cause.
	cleanupErrs := le.CleanupErrors()
	require.Len(t, cleanupErrs, 1)
	require.Same(t, cleanupErr, cleanupErrs[0].Err)
	require.Equal(t, "failing", cleanupErrs[0].Step.Name)
	require.Empty(t, le.FinalizeErrors())

	// Completed steps show execute happened and teardown phases ran.
	var stages []string
	for _, s := range le.CompletedSteps() {
		stages = append(stages, string(s.Stage))
	}
	require.Equal(t, []string{
		"prepare", "prepare", "prepare",
		"execute",
		"finalize", "finalize", "finalize",
		"cleanup", "cleanup", "cleanup", "cleanup", "cleanup",
	}, stages)

	// Errors() exposes the cause first, satisfying the MultiError view.
	require.Same(t, errActionSentinel, le.Errors()[0])
}

func TestLifecycle_BeforeFailureStopsForwardPhases(t *testing.T) {
	var log []string
	cmd := lifecycleChain(&log, true, nil)
	subBeforeErr := errors.New("sub prepare failed")
	// sub Before fails: root Before already registered its cleanup.
	cmd.Commands[0].Before = func(ctx context.Context, c *Command) (context.Context, error) {
		log = append(log, "before:sub")
		return ctx, subBeforeErr
	}

	err := cmd.Run(context.Background(), []string{"app", "sub", "leaf"})
	le := lifecycleErr(t, err)
	require.ErrorIs(t, err, subBeforeErr)

	require.Equal(t, []string{
		"before:root", "before:sub",
		// sub/leaf prepare and leaf execute never happen;
		// only the successfully prepared root finalizes and cleans up.
		"after:root", "cleanup:root-before",
	}, log)

	steps := map[string]bool{}
	for _, s := range le.CompletedSteps() {
		steps[string(s.Stage)+":"+s.Command] = true
	}
	require.True(t, steps["prepare:app"])
	require.False(t, steps["prepare:app sub leaf"])
	require.False(t, steps["execute:app sub leaf"])
	require.False(t, steps["finalize:app sub leaf"])
	require.True(t, steps["finalize:app"])
}

func TestLifecycle_ContextCancellationStopsForwardPhases(t *testing.T) {
	var log []string
	ctx, cancel := context.WithCancel(context.Background())

	root := &Command{
		Name:            "app",
		EnableLifecycle: true,
		Writer:          discardWriter{},
		ErrWriter:       discardWriter{},
		Before: func(ctx context.Context, c *Command) (context.Context, error) {
			log = append(log, "before:root")
			require.True(t, RegisterCleanup(ctx, "root", func(context.Context) error {
				log = append(log, "cleanup:root")
				return nil
			}))
			cancel()
			return ctx, nil
		},
		After: func(context.Context, *Command) error {
			log = append(log, "after:root")
			return nil
		},
		Commands: []*Command{{
			Name: "sub",
			Before: func(context.Context, *Command) (context.Context, error) {
				t.Error("sub Before must not run after cancellation")
				return nil, nil
			},
			Action: func(context.Context, *Command) error {
				t.Error("sub Action must not run after cancellation")
				return nil
			},
		}},
	}

	err := root.Run(ctx, []string{"app", "sub"})
	le := lifecycleErr(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.Same(t, context.Canceled, le.Cause())

	require.Equal(t, []string{"before:root", "after:root", "cleanup:root"}, log)
}

func TestLifecycle_PreCanceledContextSkipsAllForwardPhases(t *testing.T) {
	var ran bool
	root := &Command{
		Name:            "app",
		EnableLifecycle: true,
		Writer:          discardWriter{},
		ErrWriter:       discardWriter{},
		Before: func(context.Context, *Command) (context.Context, error) {
			ran = true
			return nil, nil
		},
		Action: func(context.Context, *Command) error {
			ran = true
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := root.Run(ctx, []string{"app"})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, ran)
}

func TestLifecycle_PanicInActionRecoveredAndTornDown(t *testing.T) {
	var log []string
	cmd := lifecycleChain(&log, true, func(context.Context, *Command) error {
		panic("boom")
	})

	err := cmd.Run(context.Background(), []string{"app", "sub", "leaf"})
	le := lifecycleErr(t, err)

	var pe *LifecyclePanicError
	require.ErrorAs(t, err, &pe)
	require.Same(t, pe, le.Cause())
	require.Equal(t, "boom", pe.Value)
	require.Equal(t, LifecycleExecute, pe.Step.Stage)
	require.NotEmpty(t, pe.Stack)

	// Finalize and cleanup still completed in order after the panic. The
	// action registered its cleanup before panicking, so it runs too.
	require.Equal(t, []string{
		"before:root", "before:sub", "before:leaf", "action:leaf",
		"after:leaf", "after:sub", "after:root",
		"cleanup:leaf-action", "cleanup:leaf-before",
		"cleanup:sub-before", "cleanup:root-before",
	}, log)
}

func TestLifecycle_PanicInBeforeStopsForwardPhases(t *testing.T) {
	var log []string
	cmd := lifecycleChain(&log, true, nil)
	cmd.Commands[0].Commands[0].Before = func(context.Context, *Command) (context.Context, error) {
		log = append(log, "before:leaf")
		panic("leaf panic")
	}

	err := cmd.Run(context.Background(), []string{"app", "sub", "leaf"})
	le := lifecycleErr(t, err)

	var pe *LifecyclePanicError
	require.ErrorAs(t, err, &pe)
	require.Same(t, pe, le.Cause())
	require.Equal(t, LifecyclePrepare, pe.Step.Stage)
	require.Equal(t, "app sub leaf", pe.Step.Command)

	// root and sub prepared successfully, so both finalize in reverse; the
	// panicking leaf never executes.
	require.Equal(t, []string{
		"before:root", "before:sub", "before:leaf",
		"after:sub", "after:root",
		"cleanup:sub-before", "cleanup:root-before",
	}, log)
}

func TestLifecycle_PanicInFinalizeDoesNotBlockTeardown(t *testing.T) {
	var log []string
	cmd := lifecycleChain(&log, true, nil)
	cmd.Commands[0].Commands[0].After = func(context.Context, *Command) error {
		log = append(log, "after:leaf")
		panic("after panic")
	}

	err := cmd.Run(context.Background(), []string{"app", "sub", "leaf"})
	le := lifecycleErr(t, err)

	// Action succeeded: no primary cause, but the panic is still visible.
	require.Nil(t, le.Cause())
	finalizeErrs := le.FinalizeErrors()
	require.Len(t, finalizeErrs, 1)
	var pe *LifecyclePanicError
	require.ErrorAs(t, finalizeErrs[0].Err, &pe)
	require.Equal(t, "after panic", pe.Value)
	require.Equal(t, LifecycleFinalize, pe.Step.Stage)

	// Remaining After hooks and all cleanups still ran.
	require.Equal(t, []string{
		"before:root", "before:sub", "before:leaf", "action:leaf",
		"after:leaf", "after:sub", "after:root",
		"cleanup:leaf-action", "cleanup:leaf-before",
		"cleanup:sub-before", "cleanup:root-before",
	}, log)
}

func TestLifecycle_HelpEntrySkipsHooksAndStateDoesNotLeak(t *testing.T) {
	var hooks []string
	var entries []LifecycleEntry
	root := &Command{
		Name:            "app",
		EnableLifecycle: true,
		Writer:          discardWriter{},
		ErrWriter:       discardWriter{},
		Before: func(context.Context, *Command) (context.Context, error) {
			hooks = append(hooks, "before")
			return nil, nil
		},
		After: func(context.Context, *Command) error {
			hooks = append(hooks, "after")
			return nil
		},
		Commands: []*Command{{Name: "sub", Action: func(context.Context, *Command) error { return nil }}},
	}
	root.LifecycleObserver = func(ev LifecycleEvent) {
		if ev.Type == LifecycleEnd {
			entries = append(entries, ev.Entry)
		}
		if ev.Type == LifecycleStageBegin && ev.Step.Stage == LifecycleExecute && ev.Step.Name != "" {
			hooks = append(hooks, "execute:"+ev.Step.Name)
		}
	}

	// --help renders help without prepare/finalize.
	require.NoError(t, root.Run(context.Background(), []string{"app", "--help"}))
	require.Equal(t, []string{"execute:help"}, hooks)
	require.Equal(t, []LifecycleEntry{LifecycleEntryHelp}, entries)

	// The same command answers a real invocation afterwards with no leak.
	require.NoError(t, root.Run(context.Background(), []string{"app", "sub"}))
	require.Equal(t, []string{"execute:help", "before", "after"}, hooks)
	require.Equal(t, []LifecycleEntry{LifecycleEntryHelp, LifecycleEntryRun}, entries)
}

func TestLifecycle_CompletionEntryRunsPrepareAndCleanupButNotAfter(t *testing.T) {
	var log []string
	var completed bool
	root := &Command{
		Name:                  "app",
		EnableLifecycle:       true,
		EnableShellCompletion: true,
		Writer:                discardWriter{},
		ErrWriter:             discardWriter{},
		ShellComplete: func(context.Context, *Command) {
			completed = true
		},
		Before: func(ctx context.Context, c *Command) (context.Context, error) {
			log = append(log, "before")
			require.True(t, RegisterCleanup(ctx, "c", func(context.Context) error {
				log = append(log, "cleanup")
				return nil
			}))
			return ctx, nil
		},
		After: func(context.Context, *Command) error {
			log = append(log, "after")
			return nil
		},
	}

	err := root.Run(context.Background(), []string{"app", "--generate-shell-completion"})
	require.NoError(t, err)
	require.True(t, completed, "ShellComplete must run")
	require.Equal(t, []string{"before", "cleanup"}, log, "After must not run on completion")
}

func TestLifecycle_CompletionEntryBeforeFailure(t *testing.T) {
	var completed bool
	beforeErr := errors.New("completion prepare failed")
	root := &Command{
		Name:                  "app",
		EnableLifecycle:       true,
		EnableShellCompletion: true,
		Writer:                discardWriter{},
		ErrWriter:             discardWriter{},
		ShellComplete: func(context.Context, *Command) {
			completed = true
		},
		Before: func(context.Context, *Command) (context.Context, error) {
			return nil, beforeErr
		},
	}

	err := root.Run(context.Background(), []string{"app", "--generate-shell-completion"})
	le := lifecycleErr(t, err)
	require.False(t, completed, "completion rendering must not run when prepare fails")
	require.ErrorIs(t, err, beforeErr)
	require.Equal(t, LifecycleEntryCompletion, le.Entry())
}

func TestLifecycle_VersionEntry(t *testing.T) {
	var executed string
	root := &Command{
		Name:            "app",
		Version:         "1.2.3",
		EnableLifecycle: true,
		Writer:          discardWriter{},
		ErrWriter:       discardWriter{},
		Before: func(context.Context, *Command) (context.Context, error) {
			t.Error("Before must not run for --version")
			return nil, nil
		},
	}
	root.LifecycleObserver = func(ev LifecycleEvent) {
		if ev.Type == LifecycleStageBegin && ev.Step.Stage == LifecycleExecute {
			executed = ev.Step.Name
		}
	}

	require.NoError(t, root.Run(context.Background(), []string{"app", "--version"}))
	require.Equal(t, "version", executed)
}

func TestLifecycle_NoDuplicateAfterWithMultipleFrames(t *testing.T) {
	// Regression guard: when lifecycle is enabled, the legacy per-frame After
	// defers are suppressed and the orchestrator owns After execution.
	var afters []string
	after := func(who string) AfterFunc {
		return func(context.Context, *Command) error {
			afters = append(afters, who)
			return nil
		}
	}
	root := &Command{
		Name:            "app",
		EnableLifecycle: true,
		Writer:          discardWriter{},
		ErrWriter:       discardWriter{},
		After:           after("root"),
		Commands: []*Command{{
			Name:   "sub",
			After:  after("sub"),
			Action: func(context.Context, *Command) error { return nil },
		}},
	}

	require.NoError(t, root.Run(context.Background(), []string{"app", "sub"}))
	require.Equal(t, []string{"sub", "root"}, afters)

	require.NoError(t, root.Run(context.Background(), []string{"app", "sub"}))
	require.Equal(t, []string{"sub", "root", "sub", "root"}, afters)
}

func TestLifecycle_CleanupErrorAfterSuccessIsReported(t *testing.T) {
	cleanupErr := errors.New("close failed")
	root := &Command{
		Name:            "app",
		EnableLifecycle: true,
		Writer:          discardWriter{},
		ErrWriter:       discardWriter{},
		Action: func(ctx context.Context, c *Command) error {
			require.True(t, RegisterCleanup(ctx, "res", func(context.Context) error {
				return cleanupErr
			}))
			return nil
		},
	}

	err := root.Run(context.Background(), []string{"app"})
	le := lifecycleErr(t, err)
	require.NoError(t, le.Cause())
	require.Len(t, le.CleanupErrors(), 1)
	require.ErrorIs(t, le.CleanupErrors()[0].Err, cleanupErr)
	require.True(t, strings.Contains(err.Error(), "cleanup \"res\""))
	require.True(t, strings.Contains(err.Error(), "close failed"))
}

func TestLifecycle_ErrorStringShowsRelationship(t *testing.T) {
	root := &Command{
		Name:            "app",
		EnableLifecycle: true,
		Writer:          discardWriter{},
		ErrWriter:       discardWriter{},
		Action: func(ctx context.Context, c *Command) error {
			RegisterCleanup(ctx, "res", func(context.Context) error {
				return errors.New("teardown trouble")
			})
			return errors.New("primary trouble")
		},
	}

	err := root.Run(context.Background(), []string{"app"})
	require.Error(t, err)
	msg := err.Error()
	require.True(t, strings.Contains(msg, "primary trouble"), msg)
	require.True(t, strings.Contains(msg, "teardown trouble"), msg)

	// Standard exit handling recognizes the aggregate as a MultiError.
	var me MultiError
	require.True(t, errors.As(err, &me))
	require.Len(t, me.Errors(), 2)
}

func TestLifecycle_CleanupReceivesContextAndRunsOnUsageError(t *testing.T) {
	// A usage (parse) error happens before prepare, so no user cleanups exist
	// yet; the invocation must still report a lifecycle error with the cause.
	root := &Command{
		Name:            "app",
		EnableLifecycle: true,
		Writer:          discardWriter{},
		ErrWriter:       discardWriter{},
		Flags:           []Flag{&IntFlag{Name: "n"}},
		Action:          func(context.Context, *Command) error { return nil },
	}

	err := root.Run(context.Background(), []string{"app", "--n", "not-an-int"})
	le := lifecycleErr(t, err)
	require.Error(t, le.Cause())
	require.Equal(t, LifecycleEntryRun, le.Entry())
	require.Empty(t, le.CompletedSteps())
}

// keep io referenced for documentation-level clarity on writer options.
var _ io.Writer = discardWriter{}
