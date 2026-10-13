package cli

import (
	"fmt"
	"reflect"
)

// MergeStrategy selects how the contributions of a flag are combined
// before the final value is committed.
//
// When a flag declares a merge strategy, parsing becomes transactional:
// values from the default, the configured Sources (environment variables,
// files, map sources) and repeated command-line occurrences (including
// values inherited from parent commands or given through an alias) are
// first buffered in a pending state. Decoding, merging, normalization and
// validation only run once the whole command line has been parsed
// successfully, and the resulting value is published atomically. A failed
// conversion or validation therefore never leaves a partially written
// value in the flag or its Destination.
//
// Contributions are layered in this order, each later layer having higher
// precedence:
//
//	default value < Sources (first source in the chain that resolves) < command-line occurrences
//
// The zero value MergeStrategyDefault keeps the historical, non
// transactional parsing behavior.
type MergeStrategy uint8

const (
	// MergeStrategyDefault keeps the built-in parsing behavior. The flag
	// does not buffer values and parses eagerly as usual.
	MergeStrategyDefault MergeStrategy = iota

	// MergeStrategyOverride reduces all contributions to the last decoded
	// occurrence. For slice and map flags this replaces the whole value
	// rather than accumulating into it.
	MergeStrategyOverride

	// MergeStrategyAppend accumulates all contributions. Slice flags keep
	// every element and map flags overlay entries (a key given later wins).
	// Flags whose value is neither a slice nor a map behave like
	// MergeStrategyOverride.
	MergeStrategyAppend

	// MergeStrategyDedup behaves like MergeStrategyAppend, but elements
	// already present in a slice value are not added again.
	MergeStrategyDedup

	// MergeStrategyCustom combines the decoded contributions with the
	// flag's MergeFunc.
	MergeStrategyCustom
)

// MergeSemanticsText returns a short, human readable description of how
// repeated values of this flag are combined. It returns an empty string
// when no merge strategy is configured.
func (s MergeStrategy) MergeSemanticsText() string {
	switch s {
	case MergeStrategyOverride:
		return "repeatable; the last value overrides earlier ones"
	case MergeStrategyAppend:
		return "repeatable; values are appended"
	case MergeStrategyDedup:
		return "repeatable; values are appended and duplicates are ignored"
	case MergeStrategyCustom:
		return "repeatable; values are combined using a custom rule"
	default:
		return ""
	}
}

// MergeSemanticsFlag is implemented by flags that declare how repeated
// values combine. Help and shell completion use it to describe the merge
// semantics of the flag.
type MergeSemanticsFlag interface {
	GetMergeStrategy() MergeStrategy
	MergeSemanticsText() string
}

// transactionalFlag is implemented by flags that buffer parsed values in a
// pending state and publish them only after the full command parse has been
// accepted.
type transactionalFlag interface {
	CommitPending() error
}

// pendingFlagInput is a buffered raw contribution together with the
// multi-value parsing configuration (separators) of the command that
// parsed it. Persistent flags can be parsed by several commands in the
// same chain, so the configuration must travel with each occurrence.
type pendingFlagInput struct {
	raw string
	cfg *multiValueParsingConfig
}

// flagValueTransaction holds the pending contributions of a transactional
// flag until CommitPending publishes the merged value.
type flagValueTransaction[T any] struct {
	strategy    MergeStrategy
	mergeFunc   func([]T) (T, error)
	normalize   func(T) (T, error)
	dest        *T
	cliValues   []pendingFlagInput
	source      *pendingFlagInput
	sourcesDone bool
	committed   bool
}

func (f *FlagBase[T, C, V]) transactionConfigured() bool {
	return f.Merge != MergeStrategyDefault || f.Normalize != nil
}

// snapshotConfig captures the parsing configuration of the command that is
// currently feeding the flag.
func (f *FlagBase[T, C, V]) snapshotConfig() *multiValueParsingConfig {
	if f.multiValueConfig == nil {
		return nil
	}
	cfg := *f.multiValueConfig
	return &cfg
}

// beginTransaction initializes the pending state for one parse run.
func (f *FlagBase[T, C, V]) beginTransaction() (*flagValueTransaction[T], error) {
	if f.Merge == MergeStrategyCustom && f.MergeFunc == nil {
		return nil, fmt.Errorf(
			"flag %q uses MergeStrategyCustom but does not define a MergeFunc",
			f.Name,
		)
	}

	// A Command can be reused across runs, so transactional state always
	// starts the parse run fresh.
	f.count = 0
	f.hasBeenSet = false
	f.fromSource = false

	txn := &flagValueTransaction[T]{
		strategy:  f.Merge,
		mergeFunc: f.MergeFunc,
		normalize: f.Normalize,
	}

	if f.Destination != nil {
		txn.dest = f.Destination
	} else {
		txn.dest = new(T)
	}

	return txn, nil
}

// stageValue buffers one raw command-line occurrence without touching the
// published value or the Destination.
func (f *FlagBase[T, C, V]) stageValue(val string) {
	f.txn.cliValues = append(f.txn.cliValues, pendingFlagInput{
		raw: val,
		cfg: f.snapshotConfig(),
	})
	f.hasBeenSet = true
}

// stageSourceValue buffers the value resolved from the flag's Sources
// chain. Sources are consulted at most once per parse run.
func (f *FlagBase[T, C, V]) stageSourceValue() {
	txn := f.txn
	if txn.sourcesDone {
		return
	}
	txn.sourcesDone = true

	kind := reflect.Invalid
	if ty := reflect.TypeOf(f.Value); ty != nil {
		kind = ty.Kind()
	}

	// An empty value is a value only for a string, and reads as false for
	// a bool. Any other kind has nothing to parse from it, so an empty
	// source is skipped. This mirrors the regular PostParse behavior.
	emptyIsValue := kind == reflect.String || kind == reflect.Bool

	for _, source := range f.Sources.Chain {
		val, found := source.Lookup()
		if !found || (val == "" && !emptyIsValue) {
			continue
		}

		if val == "" && kind == reflect.Bool {
			val = "false"
		}

		txn.source = &pendingFlagInput{raw: val, cfg: f.snapshotConfig()}
		f.hasBeenSet = true
		f.fromSource = true
		return
	}
}

// defaultOccurrence returns the configured default value when it carries
// data and therefore has to participate in the merge.
func (f *FlagBase[T, C, V]) defaultOccurrence() (T, bool) {
	rv := reflect.ValueOf(f.Value)
	if !rv.IsValid() {
		return f.Value, false
	}

	switch rv.Kind() {
	case reflect.Slice, reflect.Map, reflect.Array:
		if rv.Len() == 0 {
			return f.Value, false
		}
	case reflect.Ptr, reflect.Interface:
		if rv.IsNil() {
			return f.Value, false
		}
	default:
		if rv.IsZero() {
			return f.Value, false
		}
	}

	return f.Value, true
}

// decodePending converts one buffered raw value into a decoded T using a
// throwaway value holder, so a decoding failure cannot mutate previously
// decoded contributions, the published value or the Destination.
func (f *FlagBase[T, C, V]) decodePending(input pendingFlagInput) (T, error) {
	raw := input.raw
	var zero T

	// A GenericFlag has an interface T (cli.Value); decode through a
	// shallow copy of the configured prototype value.
	var tProbe T
	if reflect.TypeOf(&tProbe).Elem().Kind() == reflect.Interface {
		proto := reflect.ValueOf(f.Value)
		if !proto.IsValid() || proto.Kind() != reflect.Ptr || proto.IsNil() {
			return zero, fmt.Errorf(
				"cannot decode %[1]q: generic flag %[2]q requires a non-nil pointer prototype Value",
				raw, f.Name,
			)
		}
		clone := reflect.New(proto.Elem().Type())
		clone.Elem().Set(proto.Elem())
		gv, ok := clone.Interface().(Value)
		if !ok {
			return zero, fmt.Errorf(
				"cannot decode %[1]q: prototype value of generic flag %[2]q does not implement Value",
				raw, f.Name,
			)
		}
		if err := gv.Set(raw); err != nil {
			return zero, err
		}
		return any(gv).(T), nil
	}

	p := new(T)
	v := f.creator.Create(zero, p, f.Config)
	if cf, ok := v.(multiValueParsingConfigSetter); ok {
		switch {
		case input.cfg != nil:
			cf.setMultiValueParsingConfig(*input.cfg)
		case f.multiValueConfig != nil:
			cf.setMultiValueParsingConfig(*f.multiValueConfig)
		}
	}

	if err := v.Set(raw); err != nil {
		return zero, err
	}

	typed, ok := v.Get().(T)
	if !ok {
		return zero, fmt.Errorf("decoded value %[1]T is not of type %[2]T for flag %[3]q", v.Get(), zero, f.Name)
	}
	return typed, nil
}

// CommitPending decodes all buffered contributions, applies the merge
// strategy, normalization and validation, and publishes the final value
// atomically. It is safe to call more than once: subsequent calls are
// no-ops.
func (f *FlagBase[T, C, V]) CommitPending() error {
	txn := f.txn
	if txn == nil || txn.committed {
		return nil
	}

	occurrences := make([]T, 0, len(txn.cliValues)+2)

	if txn.strategy == MergeStrategyDefault {
		// A transaction enabled solely through Normalize preserves the
		// historical precedence: command-line values replace Sources,
		// which replace the default. Within the winning layer, repeated
		// values keep their type-native accumulation (slices append,
		// scalars take the last value).
		switch {
		case len(txn.cliValues) > 0:
			for _, raw := range txn.cliValues {
				decoded, err := f.decodePending(raw)
				if err != nil {
					return err
				}
				occurrences = append(occurrences, decoded)
			}
		case txn.source != nil:
			decoded, err := f.decodePending(*txn.source)
			if err != nil {
				return f.sourceDecodeError(txn.source.raw, err)
			}
			occurrences = append(occurrences, decoded)
		default:
			if dv, ok := f.defaultOccurrence(); ok {
				occurrences = append(occurrences, dv)
			}
		}

		// Append accumulates slice/map occurrences and naturally falls
		// back to last-wins for scalar values.
		merged, err := mergeFlagOccurrences(MergeStrategyAppend, nil, occurrences)
		if err != nil {
			return err
		}
		return f.publishPending(txn, merged)
	}

	if dv, ok := f.defaultOccurrence(); ok {
		occurrences = append(occurrences, dv)
	}

	if txn.source != nil {
		decoded, err := f.decodePending(*txn.source)
		if err != nil {
			return f.sourceDecodeError(txn.source.raw, err)
		}
		occurrences = append(occurrences, decoded)
	}

	for _, raw := range txn.cliValues {
		decoded, err := f.decodePending(raw)
		if err != nil {
			return err
		}
		occurrences = append(occurrences, decoded)
	}

	merged, err := mergeFlagOccurrences(txn.strategy, txn.mergeFunc, occurrences)
	if err != nil {
		return err
	}

	return f.publishPending(txn, merged)
}

func (f *FlagBase[T, C, V]) sourceDecodeError(raw string, cause error) error {
	return fmt.Errorf(
		"could not parse %[1]q from value source for flag %[2]s: %[3]w",
		raw, f.Name, cause,
	)
}

// publishPending normalizes, validates and atomically publishes the merged
// value.
func (f *FlagBase[T, C, V]) publishPending(txn *flagValueTransaction[T], merged T) error {
	if txn.normalize != nil {
		normalized, err := txn.normalize(merged)
		if err != nil {
			return err
		}
		merged = normalized
	}

	// Validation only applies to values that were provided during this
	// parse run. Unset flags keep their default, which is validated up
	// front via ValidateDefaults, mirroring the non transactional
	// behavior.
	if f.Validator != nil && f.hasBeenSet {
		if err := f.Validator(merged); err != nil {
			return err
		}
	}

	// Publish the result in a single assignment. Creator implementations
	// reset the destination and copy the merged value, so the Destination
	// never observes an intermediate state.
	f.value = f.creator.Create(merged, txn.dest, f.Config)
	if f.multiValueConfig != nil {
		f.setMultiValueParsingConfig(*f.multiValueConfig)
	}

	txn.committed = true
	return nil
}

// mergeFlagOccurrences reduces decoded contributions according to the
// configured merge strategy.
func mergeFlagOccurrences[T any](
	strategy MergeStrategy,
	custom func([]T) (T, error),
	occurrences []T,
) (T, error) {
	var zero T

	if len(occurrences) == 0 {
		return zero, nil
	}

	switch strategy {
	case MergeStrategyCustom:
		if custom == nil {
			return zero, fmt.Errorf("custom merge strategy requires a non-nil MergeFunc")
		}
		return custom(occurrences)
	case MergeStrategyAppend, MergeStrategyDedup:
		if merged, ok := concatenateOccurrences(occurrences, strategy == MergeStrategyDedup); ok {
			return merged, nil
		}
		// Non-slice/map values cannot be accumulated: the last occurrence
		// wins, just like MergeStrategyOverride.
		return occurrences[len(occurrences)-1], nil
	case MergeStrategyOverride:
		fallthrough
	default:
		return occurrences[len(occurrences)-1], nil
	}
}

// concatenateOccurrences merges slice or map contributions. It reports
// ok=false when the values are neither slices nor maps.
func concatenateOccurrences[T any](occurrences []T, dedup bool) (merged T, ok bool) {
	var zero T

	ty := reflect.TypeOf(occurrences[0])
	if ty == nil {
		return zero, false
	}

	switch ty.Kind() {
	case reflect.Slice:
		total := 0
		for _, occurrence := range occurrences {
			if rv := reflect.ValueOf(occurrence); rv.IsValid() {
				total += rv.Len()
			}
		}

		out := reflect.MakeSlice(ty, 0, total)
		for _, occurrence := range occurrences {
			rv := reflect.ValueOf(occurrence)
			if !rv.IsValid() {
				continue
			}
			for i := 0; i < rv.Len(); i++ {
				elem := rv.Index(i)
				if dedup && sliceContains(out, elem) {
					continue
				}
				out = reflect.Append(out, elem)
			}
		}

		return out.Interface().(T), true

	case reflect.Map:
		out := reflect.MakeMap(ty)
		for _, occurrence := range occurrences {
			rv := reflect.ValueOf(occurrence)
			if !rv.IsValid() || rv.IsNil() {
				continue
			}
			iter := rv.MapRange()
			for iter.Next() {
				out.SetMapIndex(iter.Key(), iter.Value())
			}
		}

		return out.Interface().(T), true
	}

	return zero, false
}

func sliceContains(slice reflect.Value, elem reflect.Value) bool {
	for i := 0; i < slice.Len(); i++ {
		if reflect.DeepEqual(slice.Index(i).Interface(), elem.Interface()) {
			return true
		}
	}
	return false
}

// GetMergeStrategy returns the configured merge strategy of the flag.
func (f *FlagBase[T, C, V]) GetMergeStrategy() MergeStrategy {
	return f.Merge
}

// MergeSemanticsText returns a short description of how repeated values of
// this flag are combined.
func (f *FlagBase[T, C, V]) MergeSemanticsText() string {
	return f.Merge.MergeSemanticsText()
}
