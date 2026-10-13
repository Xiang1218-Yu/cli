package cli

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
)

// LifecycleStage names one ordered phase of a command invocation.
type LifecycleStage string

const (
	// LifecyclePrepare is the preparation phase: the Before hooks of the
	// command chain run in order from the root command to the invoked
	// (innermost) command.
	LifecyclePrepare LifecycleStage = "prepare"

	// LifecycleExecute is the execution phase: the Action of the invoked
	// command (or the help/completion rendering for those entries).
	LifecycleExecute LifecycleStage = "execute"

	// LifecycleFinalize is the finalization phase: the After hooks of
	// commands whose Before hook completed run in the reverse order, from
	// the invoked command back to the root.
	LifecycleFinalize LifecycleStage = "finalize"

	// LifecycleCleanup is the resource cleanup phase: cleanup functions
	// registered with RegisterCleanup run exactly once, in the reverse
	// order in which they were registered.
	LifecycleCleanup LifecycleStage = "cleanup"
)

// LifecycleEntry identifies how an invocation entered the command graph.
type LifecycleEntry string

const (
	// LifecycleEntryRun is a normal command invocation.
	LifecycleEntryRun LifecycleEntry = "run"

	// LifecycleEntryHelp is an invocation handled by the built-in help
	// (the --help/-h flag or the help command action).
	LifecycleEntryHelp LifecycleEntry = "help"

	// LifecycleEntryCompletion is a shell completion request
	// (--generate-shell-completion).
	LifecycleEntryCompletion LifecycleEntry = "completion"

	// LifecycleEntryVersion is an invocation handled by the --version flag.
	LifecycleEntryVersion LifecycleEntry = "version"
)

// CleanupFunc is a resource-release action registered for one command
// invocation through RegisterCleanup. It runs exactly once during the
// LifecycleCleanup phase, in the reverse order of registration, and still
// runs when the invocation was canceled, a hook returned an error, or a
// hook panicked.
//
// The context passed to a CleanupFunc may already be canceled; cleanup
// functions must release their resources regardless.
type CleanupFunc func(context.Context) error

// LifecycleStep identifies one step of an invocation: a hook phase on a
// specific command, or a named cleanup function.
type LifecycleStep struct {
	// Command is the full name (command path) of the command owning the
	// step, for example "app servers start".
	Command string

	// Stage is the lifecycle phase of the step.
	Stage LifecycleStage

	// Name is the name passed to RegisterCleanup for LifecycleCleanup
	// steps, or "help"/"completion"/"version" for those execute entries.
	// It is empty for ordinary prepare/execute/finalize steps.
	Name string
}

func (s LifecycleStep) String() string {
	switch s.Stage {
	case LifecycleCleanup:
		return fmt.Sprintf("cleanup %q", s.Name)
	case LifecycleExecute:
		if s.Name != "" {
			return fmt.Sprintf("execute %s (%s)", s.Command, s.Name)
		}
		return fmt.Sprintf("execute %s", s.Command)
	default:
		return fmt.Sprintf("%s %s", s.Stage, s.Command)
	}
}

// LifecycleHookError pairs a hook error (or recovered panic) with the step
// that produced it.
type LifecycleHookError struct {
	// Step is the lifecycle step that produced the error.
	Step LifecycleStep
	// Err is the error returned by the hook, or a *LifecyclePanicError
	// recovered from a panicking hook.
	Err error
}

func (e LifecycleHookError) Error() string {
	return fmt.Sprintf("%s: %v", e.Step, e.Err)
}

// Unwrap returns the underlying hook error.
func (e LifecycleHookError) Unwrap() error { return e.Err }

// LifecyclePanicError wraps a value recovered from a panicking lifecycle
// hook. In lifecycle-enabled invocations a panic does not crash the
// process: it is recovered, teardown (finalize and cleanup) continues, and
// the recovered value is returned to the caller wrapped in a
// LifecyclePanicError, either as the primary error or alongside one.
type LifecyclePanicError struct {
	// Step is the lifecycle step that panicked.
	Step LifecycleStep
	// Value is the recovered panic value.
	Value any
	// Stack is the captured stack trace at the recovery point.
	Stack []byte
}

func (e *LifecyclePanicError) Error() string {
	return fmt.Sprintf("panic during %s: %v", e.Step, e.Value)
}

// LifecycleError is returned from a lifecycle-enabled invocation when the
// primary forward phase failed or any finalize/cleanup hook failed.
//
// The primary error is available from Unwrap (or Cause), so errors.Is and
// errors.As keep recognizing context cancellation, ExitCoder values and
// sentinel errors. Errors produced while finalizing and cleaning up are
// kept separately through FinalizeErrors and CleanupErrors: they never
// replace the primary error.
type LifecycleError struct {
	entry        LifecycleEntry
	cause        error
	completed    []LifecycleStep
	finalizeErrs []LifecycleHookError
	cleanupErrs  []LifecycleHookError
}

func (e *LifecycleError) Error() string {
	var b strings.Builder
	if e.cause != nil {
		fmt.Fprintf(&b, "lifecycle %s failed: %v", e.entry, e.cause)
	} else {
		fmt.Fprintf(&b, "lifecycle %s finished with teardown errors", e.entry)
	}
	for _, fe := range e.finalizeErrs {
		fmt.Fprintf(&b, "\n  %s", fe.Error())
	}
	for _, ce := range e.cleanupErrs {
		fmt.Fprintf(&b, "\n  %s", ce.Error())
	}
	return b.String()
}

// Unwrap returns the primary error: the context error on cancellation, the
// error returned by the failing Before or Action, or the recovered panic.
// It returns nil when only teardown hooks failed.
func (e *LifecycleError) Unwrap() error { return e.cause }

// Cause returns the primary error of the invocation; see Unwrap.
func (e *LifecycleError) Cause() error { return e.cause }

// Entry reports how the invocation entered the command graph.
func (e *LifecycleError) Entry() LifecycleEntry { return e.entry }

// CompletedSteps returns the steps that executed, in execution order. It
// includes every Before hook that ran, the execute step when it ran, every
// After hook that ran, and every cleanup function that ran, regardless of
// whether they returned an error or panicked.
func (e *LifecycleError) CompletedSteps() []LifecycleStep {
	return append([]LifecycleStep(nil), e.completed...)
}

// FinalizeErrors returns errors produced by After hooks, in execution
// order (innermost command first).
func (e *LifecycleError) FinalizeErrors() []LifecycleHookError {
	return append([]LifecycleHookError(nil), e.finalizeErrs...)
}

// CleanupErrors returns errors produced by cleanup functions, in execution
// order (reverse registration order).
func (e *LifecycleError) CleanupErrors() []LifecycleHookError {
	return append([]LifecycleHookError(nil), e.cleanupErrs...)
}

// Errors returns the primary error followed by all finalize and cleanup
// errors, omitting nils. It makes LifecycleError satisfy the package's
// MultiError interface.
func (e *LifecycleError) Errors() []error {
	var errs []error
	if e.cause != nil {
		errs = append(errs, e.cause)
	}
	for _, fe := range e.finalizeErrs {
		errs = append(errs, fe.Err)
	}
	for _, ce := range e.cleanupErrs {
		errs = append(errs, ce.Err)
	}
	return errs
}

// LifecycleEventType classifies a LifecycleEvent.
type LifecycleEventType string

const (
	// LifecycleStageBegin is emitted before a lifecycle step runs.
	LifecycleStageBegin LifecycleEventType = "stage_begin"
	// LifecycleStageEnd is emitted after a lifecycle step runs, carrying
	// its error (nil on success).
	LifecycleStageEnd LifecycleEventType = "stage_end"
	// LifecycleEnd is emitted once when the whole invocation finishes,
	// carrying the final *LifecycleError (nil on full success).
	LifecycleEnd LifecycleEventType = "invocation_end"
)

// LifecycleEvent is delivered to Command.LifecycleObserver as the
// invocation moves through its lifecycle.
type LifecycleEvent struct {
	// Type is the kind of event.
	Type LifecycleEventType
	// Entry is how the invocation entered the command graph.
	Entry LifecycleEntry
	// Step is the step the event belongs to. It is the zero value for
	// LifecycleEnd events.
	Step LifecycleStep
	// Err carries the step error on LifecycleStageEnd, or the final
	// invocation error on LifecycleEnd; nil otherwise.
	Err error
}

// LifecycleObserverFunc observes a lifecycle-enabled invocation. Observers
// must not panic and must return quickly; they run synchronously on the
// goroutine executing the command.
type LifecycleObserverFunc func(LifecycleEvent)

type lifecycleContextKey struct{}

// RegisterCleanup registers fn as a resource cleanup action for the current
// command invocation. Cleanup functions run exactly once during the
// LifecycleCleanup phase, in the reverse order in which they were
// registered, and still run after cancellation, hook errors and panics.
//
// name identifies the cleanup in lifecycle events and in
// LifecycleError.CompletedSteps; it may be empty.
//
// RegisterCleanup returns false (and does nothing) when lifecycle
// orchestration is not enabled for the current invocation, so the same
// hook code can run both with and without Command.EnableLifecycle.
func RegisterCleanup(ctx context.Context, name string, fn CleanupFunc) bool {
	if fn == nil {
		return false
	}
	lc := lifecycleFromContext(ctx)
	if lc == nil {
		return false
	}
	owner, _ := ctx.Value(commandContextKey).(*Command)
	lc.addCleanup(registeredCleanup{name: name, fn: fn, owner: owner})
	return true
}

type registeredCleanup struct {
	name  string
	fn    CleanupFunc
	owner *Command
}

// lifecycle is the per-invocation orchestration state. One instance is
// created by the root command frame and shared through the context across
// every command in the chain.
type lifecycle struct {
	observer LifecycleObserverFunc

	mu       sync.Mutex
	cleanups []registeredCleanup

	entry    LifecycleEntry
	begun    bool
	prepared []*Command
	steps    []LifecycleStep

	finalizeErrs []LifecycleHookError
	cleanupErrs  []LifecycleHookError

	finishOnce sync.Once
	result     error
}

func ensureLifecycle(ctx context.Context, cmd *Command) (context.Context, *lifecycle) {
	if lc := lifecycleFromContext(ctx); lc != nil {
		return ctx, lc
	}
	if cmd.parent != nil || !cmd.EnableLifecycle {
		return ctx, nil
	}
	lc := &lifecycle{entry: LifecycleEntryRun}
	if cmd.LifecycleObserver != nil {
		lc.observer = cmd.LifecycleObserver
	}
	return context.WithValue(ctx, lifecycleContextKey{}, lc), lc
}

func lifecycleFromContext(ctx context.Context) *lifecycle {
	lc, _ := ctx.Value(lifecycleContextKey{}).(*lifecycle)
	return lc
}

func (lc *lifecycle) addCleanup(c registeredCleanup) {
	lc.mu.Lock()
	lc.cleanups = append(lc.cleanups, c)
	lc.mu.Unlock()
}

func (lc *lifecycle) drainCleanups() []registeredCleanup {
	lc.mu.Lock()
	cleanups := lc.cleanups
	lc.cleanups = nil
	lc.mu.Unlock()
	return cleanups
}

// begin marks the invocation entry. It is idempotent: the entry of the
// first begin wins, so deeper frames cannot reclassify an invocation that
// an earlier frame already classified (for example as help).
func (lc *lifecycle) begin(entry LifecycleEntry) {
	if lc.begun {
		return
	}
	lc.entry = entry
	lc.begun = true
}

func (lc *lifecycle) emit(ev LifecycleEvent) {
	if lc.observer != nil {
		lc.observer(ev)
	}
}

// runHook invokes fn for step, recovering panics into *LifecyclePanicError,
// and records the step as executed.
func (lc *lifecycle) runHook(step LifecycleStep, fn func() error) (err error) {
	lc.emit(LifecycleEvent{Type: LifecycleStageBegin, Entry: lc.entry, Step: step})
	defer func() {
		if r := recover(); r != nil {
			err = &LifecyclePanicError{Step: step, Value: r, Stack: debug.Stack()}
		}
		lc.steps = append(lc.steps, step)
		lc.emit(LifecycleEvent{Type: LifecycleStageEnd, Entry: lc.entry, Step: step, Err: err})
	}()
	return fn()
}

func lifecycleStep(cmd *Command, stage LifecycleStage) LifecycleStep {
	return LifecycleStep{Command: cmd.FullName(), Stage: stage}
}

// handleHookError routes a non-panic hook error through the command's exit
// error handler, preserving the ExitCoder/OsExiter behavior of the default
// hook semantics. Panic errors pass through untouched.
func handleHookError(cmd *Command, ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*LifecyclePanicError); ok {
		return err
	}
	return cmd.handleExitCoder(ctx, err)
}

// runTerminal orchestrates the full prepare -> execute -> finalize ->
// cleanup sequence for the innermost command of a normal invocation.
func (lc *lifecycle) runTerminal(ctx context.Context, cmd *Command) (context.Context, error) {
	lc.begin(LifecycleEntryRun)
	chain := commandChain(cmd)

	printDeprecationWarnings(chain)

	cause, ctx := lc.prepareAndExecute(ctx, cmd, chain)

	return ctx, lc.finish(ctx, cmd, cause, true)
}

// runCompletionEntry orchestrates a shell completion request: the prepare
// phase runs exactly as for a normal invocation, followed by the
// completion rendering and the cleanup phase. After hooks do not run for
// completion requests, matching the default hook semantics.
func (lc *lifecycle) runCompletionEntry(ctx context.Context, cmd *Command) (context.Context, error) {
	lc.begin(LifecycleEntryCompletion)
	chain := commandChain(cmd)

	cause, ctx := lc.prepareChain(ctx, chain)
	if cause == nil {
		if err := ctx.Err(); err != nil {
			cause = err
		} else {
			step := lifecycleStep(cmd, LifecycleExecute)
			step.Name = string(LifecycleEntryCompletion)
			cause = lc.runHook(step, func() error {
				runCompletion(ctx, cmd)
				return nil
			})
		}
	}

	return ctx, lc.finish(ctx, cmd, cause, false)
}

// runHelpEntry renders help for a --help/-h request. No prepare or
// finalize hooks run for this entry, matching the default behavior;
// registered cleanups (none can exist at this point) still drain once.
func (lc *lifecycle) runHelpEntry(ctx context.Context, cmd *Command) (context.Context, error) {
	lc.begin(LifecycleEntryHelp)

	step := lifecycleStep(cmd, LifecycleExecute)
	step.Name = string(LifecycleEntryHelp)
	err := lc.runHook(step, func() error {
		return helpCommandAction(ctx, cmd)
	})

	return ctx, lc.finish(ctx, cmd, err, false)
}

// runVersionEntry renders version output for a --version request.
func (lc *lifecycle) runVersionEntry(ctx context.Context, cmd *Command) (context.Context, error) {
	lc.begin(LifecycleEntryVersion)

	step := lifecycleStep(cmd, LifecycleExecute)
	step.Name = string(LifecycleEntryVersion)
	err := lc.runHook(step, func() error {
		ShowVersion(cmd)
		return nil
	})

	return ctx, lc.finish(ctx, cmd, err, false)
}

// prepareChain runs the Before hooks from the root to the innermost
// command, stopping at the first cancellation, error or panic. Commands
// whose Before hook completed are recorded in lc.prepared so that
// finalize runs their After hooks only.
func (lc *lifecycle) prepareChain(ctx context.Context, chain []*Command) (error, context.Context) {
	lc.prepared = lc.prepared[:0]
	for _, c := range chain {
		if err := ctx.Err(); err != nil {
			return err, ctx
		}
		if c.Before == nil {
			lc.prepared = append(lc.prepared, c)
			continue
		}
		step := lifecycleStep(c, LifecyclePrepare)
		var bctx context.Context
		err := lc.runHook(step, func() error {
			var ierr error
			bctx, ierr = c.Before(ctx, c)
			return ierr
		})
		if err != nil {
			return handleHookError(c, ctx, err), ctx
		}
		if bctx != nil {
			ctx = bctx
			// The returned context may derive from a different parent, so
			// re-attach the invocation lifecycle to it.
			ctx = context.WithValue(ctx, lifecycleContextKey{}, lc)
		}
		lc.prepared = append(lc.prepared, c)
	}
	return nil, ctx
}

// prepareAndExecute runs everything up to and including the Action of the
// invoked command. A non-nil returned error is the primary error; forward
// stages that must not continue have been skipped.
func (lc *lifecycle) prepareAndExecute(ctx context.Context, cmd *Command, chain []*Command) (error, context.Context) {
	// Run the ArgValidator before any preparation, as in the default flow.
	if validator := findArgValidator(cmd); validator != nil {
		if err := ctx.Err(); err != nil {
			return err, ctx
		}
		if err := validator(ctx, cmd); err != nil {
			return cmd.handleExitCoder(ctx, err), ctx
		}
	}

	cause, ctx := lc.prepareChain(ctx, chain)
	if cause != nil {
		return cause, ctx
	}

	// Do not enter further forward phases after cancellation.
	if err := ctx.Err(); err != nil {
		return err, ctx
	}

	// Run flag actions in chain order.
	for _, c := range chain {
		tracef("running flag actions (cmd=%[1]q)", c.Name)
		if err := c.runFlagActions(ctx); err != nil {
			return handleHookError(c, ctx, err), ctx
		}
	}

	var requiredErr error
	if err := cmd.checkAllRequiredFlags(); err != nil {
		requiredErr = err
	} else if err := cmd.checkRequiredArguments(); err != nil {
		requiredErr = err
	}
	if requiredErr != nil {
		var err error
		ctx, err = cmd.handleRequiredError(ctx, requiredErr)
		return err, ctx
	}

	// Parse declared command Arguments.
	if len(cmd.Arguments) > 0 {
		rargs := cmd.Args().Slice()
		tracef("calling argparse with %[1]v", rargs)
		for _, arg := range cmd.Arguments {
			var err error
			rargs, err = arg.Parse(rargs)
			if err != nil {
				tracef("calling with %[1]v (cmd=%[2]q)", err, cmd.Name)
				if _, ok := err.(*errRequiredArguments); ok {
					ctx, err = cmd.handleRequiredError(ctx, err)
					return err, ctx
				}
				if cmd.OnUsageError != nil {
					err = cmd.OnUsageError(ctx, cmd, err, cmd.parent != nil)
				}
				return handleHookError(cmd, ctx, err), ctx
			}
		}
		cmd.parsedArgs = &stringSliceArgs{v: rargs}
	}

	if err := ctx.Err(); err != nil {
		return err, ctx
	}

	// Execute the command action.
	step := lifecycleStep(cmd, LifecycleExecute)
	err := lc.runHook(step, func() error {
		return cmd.Action(ctx, cmd)
	})
	if err != nil {
		tracef("calling handleExitCoder with %[1]v (cmd=%[2]q)", err, cmd.Name)
		return handleHookError(cmd, ctx, err), ctx
	}
	return nil, ctx
}

// finalizePrepared runs After hooks for successfully prepared commands in
// reverse chain order. A failing or panicking After is recorded but does
// not prevent the remaining After hooks or the cleanup phase from running.
func (lc *lifecycle) finalizePrepared(ctx context.Context) {
	for i := len(lc.prepared) - 1; i >= 0; i-- {
		c := lc.prepared[i]
		if c.After == nil {
			continue
		}
		step := lifecycleStep(c, LifecycleFinalize)
		err := lc.runHook(step, func() error {
			return c.After(ctx, c)
		})
		if err != nil {
			lc.finalizeErrs = append(lc.finalizeErrs, LifecycleHookError{
				Step: step,
				Err:  handleHookError(c, ctx, err),
			})
		}
	}
}

// runCleanups executes every registered cleanup exactly once, in the
// reverse order of registration. A failing or panicking cleanup is
// recorded but does not prevent the remaining cleanups from running.
func (lc *lifecycle) runCleanups(ctx context.Context, fallback *Command) {
	cleanups := lc.drainCleanups()
	for i := len(cleanups) - 1; i >= 0; i-- {
		c := cleanups[i]
		owner := c.owner
		if owner == nil {
			owner = fallback
		}
		step := LifecycleStep{
			Command: owner.FullName(),
			Stage:   LifecycleCleanup,
			Name:    c.name,
		}
		err := lc.runHook(step, func() error {
			return c.fn(ctx)
		})
		if err != nil {
			lc.cleanupErrs = append(lc.cleanupErrs, LifecycleHookError{
				Step: step,
				Err:  handleHookError(owner, ctx, err),
			})
		}
	}
}

// finish runs the teardown phases once and builds the final invocation
// error. Subsequent calls return the same error without running any hook
// again, which guarantees that After and cleanup side effects cannot be
// duplicated by repeated calls across command frames.
func (lc *lifecycle) finish(ctx context.Context, cmd *Command, cause error, runFinalize bool) error {
	lc.finishOnce.Do(func() {
		if !lc.begun {
			lc.begin(LifecycleEntryRun)
		}
		if runFinalize {
			lc.finalizePrepared(ctx)
		}
		lc.runCleanups(ctx, cmd)

		completed := append([]LifecycleStep(nil), lc.steps...)
		finalizeErrs := append([]LifecycleHookError(nil), lc.finalizeErrs...)
		cleanupErrs := append([]LifecycleHookError(nil), lc.cleanupErrs...)

		if cause == nil && len(finalizeErrs) == 0 && len(cleanupErrs) == 0 {
			lc.emit(LifecycleEvent{Type: LifecycleEnd, Entry: lc.entry})
			return
		}
		lc.result = &LifecycleError{
			entry:        lc.entry,
			cause:        cause,
			completed:    completed,
			finalizeErrs: finalizeErrs,
			cleanupErrs:  cleanupErrs,
		}
		lc.emit(LifecycleEvent{Type: LifecycleEnd, Entry: lc.entry, Err: lc.result})
	})
	return lc.result
}

// lcFinish classifies an entry that ended outside the central terminal
// orchestration (help/version/usage errors) and runs its teardown once.
// With lc == nil it returns err unchanged, preserving the default flow.
func lcFinish(lc *lifecycle, ctx context.Context, cmd *Command, entry LifecycleEntry, err error) error {
	if lc == nil {
		return err
	}
	lc.begin(entry)
	return lc.finish(ctx, cmd, err, false)
}
