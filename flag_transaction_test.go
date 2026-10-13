package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func runTransactionalCommand(t *testing.T, cmd *Command, args []string) error {
	t.Helper()
	cmd.Writer = &bytes.Buffer{}
	cmd.ErrWriter = &bytes.Buffer{}
	return cmd.Run(buildTestContext(t), args)
}

func TestMergeStrategySemanticsText(t *testing.T) {
	r := require.New(t)

	r.Empty(MergeStrategyDefault.MergeSemanticsText())
	r.Contains(MergeStrategyOverride.MergeSemanticsText(), "last value overrides")
	r.Contains(MergeStrategyAppend.MergeSemanticsText(), "appended")
	r.Contains(MergeStrategyDedup.MergeSemanticsText(), "duplicates")
	r.Contains(MergeStrategyCustom.MergeSemanticsText(), "custom rule")
}

func TestFlagTransactionOverrideScalar(t *testing.T) {
	r := require.New(t)

	var got string
	cmd := &Command{
		Name: "foo",
		Flags: []Flag{
			&StringFlag{
				Name:    "name",
				Aliases: []string{"n"},
				Merge:   MergeStrategyOverride,
				Action: func(_ context.Context, _ *Command, v string) error {
					got = v
					return nil
				},
			},
		},
	}

	r.NoError(runTransactionalCommand(t, cmd, []string{"foo", "--name", "a", "-n", "b", "--name=c"}))
	r.Equal("c", cmd.String("name"))
	r.Equal("c", got)
	r.Equal(3, cmd.Count("name"))
}

func TestFlagTransactionSourcePrecedence(t *testing.T) {
	cases := []struct {
		name     string
		envKey   string
		envValue string
		setEnv   bool
		args     []string
		expected string
	}{
		{
			name:     "default when nothing else contributes",
			envKey:   "TXN_NAME_UNSET_DEFAULT",
			setEnv:   false,
			args:     []string{"foo"},
			expected: "default-val",
		},
		{
			name:     "source overrides default",
			envKey:   "TXN_NAME_SET",
			envValue: "from-env",
			setEnv:   true,
			args:     []string{"foo"},
			expected: "from-env",
		},
		{
			name:     "command line overrides source",
			envKey:   "TXN_NAME_CLI",
			envValue: "from-env",
			setEnv:   true,
			args:     []string{"foo", "--name", "cli-val"},
			expected: "cli-val",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			if tc.setEnv {
				t.Setenv(tc.envKey, tc.envValue)
			}

			cmd := &Command{
				Name: "foo",
				Flags: []Flag{
					&StringFlag{
						Name:    "name",
						Value:   "default-val",
						Sources: EnvVars(tc.envKey),
						Merge:   MergeStrategyOverride,
					},
				},
			}

			r.NoError(runTransactionalCommand(t, cmd, tc.args))
			r.Equal(tc.expected, cmd.String("name"))
		})
	}
}

func TestFlagTransactionAppendLayersAllSources(t *testing.T) {
	r := require.New(t)

	t.Setenv("TXN_TAGS", "env1,env2")

	var got []string
	cmd := &Command{
		Name: "foo",
		Flags: []Flag{
			&StringSliceFlag{
				Name:    "tags",
				Value:   []string{"default-tag"},
				Sources: EnvVars("TXN_TAGS"),
				Merge:   MergeStrategyAppend,
				Action: func(_ context.Context, _ *Command, v []string) error {
					got = v
					return nil
				},
			},
		},
	}

	r.NoError(runTransactionalCommand(t, cmd, []string{
		"foo", "--tags", "cli1", "--tags", "cli2,cli3",
	}))

	expected := []string{"default-tag", "env1", "env2", "cli1", "cli2", "cli3"}
	r.Equal(expected, cmd.StringSlice("tags"))
	r.Equal(expected, got)
	r.Equal(2, cmd.Count("tags"))
}

func TestFlagTransactionDedup(t *testing.T) {
	r := require.New(t)

	cmd := &Command{
		Name: "foo",
		Flags: []Flag{
			&StringSliceFlag{
				Name:  "tags",
				Merge: MergeStrategyDedup,
			},
		},
	}

	r.NoError(runTransactionalCommand(t, cmd, []string{
		"foo", "--tags", "a", "--tags", "a,b", "--tags", "b", "--tags", "c,a",
	}))

	r.Equal([]string{"a", "b", "c"}, cmd.StringSlice("tags"))
}

func TestFlagTransactionDedupInts(t *testing.T) {
	r := require.New(t)

	cmd := &Command{
		Name: "foo",
		Flags: []Flag{
			&IntSliceFlag{
				Name:  "nums",
				Merge: MergeStrategyDedup,
			},
		},
	}

	r.NoError(runTransactionalCommand(t, cmd, []string{
		"foo", "--nums", "1,2", "--nums", "2", "--nums", "3,1",
	}))

	r.Equal([]int{1, 2, 3}, cmd.IntSlice("nums"))
}

func TestFlagTransactionCustomMerge(t *testing.T) {
	r := require.New(t)

	cmd := &Command{
		Name: "foo",
		Flags: []Flag{
			&IntFlag{
				Name:  "total",
				Merge: MergeStrategyCustom,
				MergeFunc: func(values []int) (int, error) {
					sum := 0
					for _, v := range values {
						sum += v
					}
					return sum, nil
				},
			},
		},
	}

	r.NoError(runTransactionalCommand(t, cmd, []string{
		"foo", "--total", "1", "--total", "2", "--total", "3",
	}))
	r.Equal(6, cmd.Int("total"))
}

func TestFlagTransactionCustomMergeCanReject(t *testing.T) {
	r := require.New(t)

	actionRan := false
	cmd := &Command{
		Name: "foo",
		Flags: []Flag{
			&StringSliceFlag{
				Name:  "tags",
				Merge: MergeStrategyCustom,
				MergeFunc: func(groups [][]string) ([]string, error) {
					count := 0
					for _, group := range groups {
						count += len(group)
					}
					if count > 2 {
						return nil, fmt.Errorf("at most two tags allowed")
					}
					flat := make([]string, 0, count)
					for _, group := range groups {
						flat = append(flat, group...)
					}
					return flat, nil
				},
			},
		},
		Action: func(context.Context, *Command) error {
			actionRan = true
			return nil
		},
	}

	err := runTransactionalCommand(t, cmd, []string{
		"foo", "--tags", "a", "--tags", "b", "--tags", "c",
	})
	r.Error(err)
	r.Contains(err.Error(), "at most two tags allowed")
	r.False(actionRan)
}

func TestFlagTransactionCustomMergeRequiresMergeFunc(t *testing.T) {
	r := require.New(t)

	cmd := &Command{
		Name: "foo",
		Flags: []Flag{
			&StringFlag{
				Name:  "name",
				Merge: MergeStrategyCustom,
			},
		},
	}

	err := runTransactionalCommand(t, cmd, []string{"foo", "--name", "a"})
	r.Error(err)
	r.Contains(err.Error(), "MergeFunc")
}

func TestFlagTransactionNormalize(t *testing.T) {
	r := require.New(t)

	cmd := &Command{
		Name: "foo",
		Flags: []Flag{
			&StringFlag{
				Name:  "name",
				Merge: MergeStrategyOverride,
				Normalize: func(v string) (string, error) {
					return strings.ToUpper(strings.TrimSpace(v)), nil
				},
			},
		},
	}

	r.NoError(runTransactionalCommand(t, cmd, []string{"foo", "--name", "  bob "}))
	r.Equal("BOB", cmd.String("name"))
}

func TestFlagTransactionNormalizeOnly(t *testing.T) {
	r := require.New(t)

	cmd := &Command{
		Name: "foo",
		Flags: []Flag{
			&StringSliceFlag{
				Name: "tags",
				Normalize: func(v []string) ([]string, error) {
					out := make([]string, 0, len(v))
					for _, tag := range v {
						out = append(out, "#"+tag)
					}
					return out, nil
				},
			},
		},
	}

	r.NoError(runTransactionalCommand(t, cmd, []string{"foo", "--tags", "a,b"}))
	r.Equal([]string{"#a", "#b"}, cmd.StringSlice("tags"))
}

func TestFlagTransactionNormalizeOnlyKeepsLegacyPrecedence(t *testing.T) {
	t.Run("cli replaces source for scalar", func(t *testing.T) {
		r := require.New(t)
		t.Setenv("TXN_NM_NAME", "env")

		cmd := &Command{
			Name: "foo",
			Flags: []Flag{
				&StringFlag{
					Name:    "name",
					Value:   "default",
					Sources: EnvVars("TXN_NM_NAME"),
					Normalize: func(v string) (string, error) {
						return strings.ToUpper(v), nil
					},
				},
			},
		}

		r.NoError(runTransactionalCommand(t, cmd, []string{"foo", "--name", "cli"}))
		r.Equal("CLI", cmd.String("name"))
	})

	t.Run("source is fallback for scalar", func(t *testing.T) {
		r := require.New(t)
		t.Setenv("TXN_NM_NAME2", "env")

		cmd := &Command{
			Name: "foo",
			Flags: []Flag{
				&StringFlag{
					Name:    "name",
					Value:   "default",
					Sources: EnvVars("TXN_NM_NAME2"),
					Normalize: func(v string) (string, error) {
						return strings.ToUpper(v), nil
					},
				},
			},
		}

		r.NoError(runTransactionalCommand(t, cmd, []string{"foo"}))
		r.Equal("ENV", cmd.String("name"))
	})

	t.Run("cli replaces source for slice", func(t *testing.T) {
		r := require.New(t)
		t.Setenv("TXN_NM_TAGS", "env1,env2")

		cmd := &Command{
			Name: "foo",
			Flags: []Flag{
				&StringSliceFlag{
					Name:    "tags",
					Sources: EnvVars("TXN_NM_TAGS"),
					Normalize: func(v []string) ([]string, error) {
						out := make([]string, 0, len(v))
						for _, tag := range v {
							out = append(out, "#"+tag)
						}
						return out, nil
					},
				},
			},
		}

		r.NoError(runTransactionalCommand(t, cmd, []string{"foo", "--tags", "cli1", "--tags", "cli2"}))
		r.Equal([]string{"#cli1", "#cli2"}, cmd.StringSlice("tags"))
	})
}

func TestFlagTransactionDecodeFailureIsAtomic(t *testing.T) {
	r := require.New(t)

	var dest []int
	actionRan := false
	cmd := &Command{
		Name: "foo",
		Flags: []Flag{
			&IntSliceFlag{
				Name:        "nums",
				Merge:       MergeStrategyAppend,
				Destination: &dest,
			},
		},
		Action: func(context.Context, *Command) error {
			actionRan = true
			return nil
		},
	}

	err := runTransactionalCommand(t, cmd, []string{
		"foo", "--nums", "1", "--nums", "2", "--nums", "nope",
	})
	r.Error(err)
	r.Contains(err.Error(), "invalid value")
	// The destination must never have been written and the action must not run.
	r.Nil(dest)
	r.False(actionRan)
	r.Empty(cmd.IntSlice("nums"))
}

func TestFlagTransactionSourceDecodeFailureIsAtomic(t *testing.T) {
	r := require.New(t)

	var dest []int
	actionRan := false
	t.Setenv("TXN_NUMS", "1,not-a-number")

	cmd := &Command{
		Name: "foo",
		Flags: []Flag{
			&IntSliceFlag{
				Name:        "nums",
				Sources:     EnvVars("TXN_NUMS"),
				Merge:       MergeStrategyAppend,
				Destination: &dest,
			},
		},
		Action: func(context.Context, *Command) error {
			actionRan = true
			return nil
		},
	}

	err := runTransactionalCommand(t, cmd, []string{"foo", "--nums", "3"})
	r.Error(err)
	r.False(actionRan)
	r.Nil(dest)
}

func TestFlagTransactionValidationFailureIsAtomic(t *testing.T) {
	r := require.New(t)

	var dest []int
	actionRan := false
	cmd := &Command{
		Name: "foo",
		Flags: []Flag{
			&IntSliceFlag{
				Name:        "nums",
				Merge:       MergeStrategyAppend,
				Destination: &dest,
				Validator: func(v []int) error {
					if len(v) > 2 {
						return errors.New("at most two numbers")
					}
					return nil
				},
			},
		},
		Action: func(context.Context, *Command) error {
			actionRan = true
			return nil
		},
	}

	err := runTransactionalCommand(t, cmd, []string{
		"foo", "--nums", "1", "--nums", "2", "--nums", "3",
	})
	r.Error(err)
	r.Contains(err.Error(), "at most two numbers")
	r.Nil(dest)
	r.False(actionRan)
}

func TestFlagTransactionNormalizeFailureIsAtomic(t *testing.T) {
	r := require.New(t)

	var dest string
	actionRan := false
	cmd := &Command{
		Name: "foo",
		Flags: []Flag{
			&StringFlag{
				Name:        "name",
				Merge:       MergeStrategyOverride,
				Destination: &dest,
				Normalize: func(v string) (string, error) {
					if v == "bad" {
						return "", errors.New("bad value")
					}
					return v, nil
				},
			},
		},
		Action: func(context.Context, *Command) error {
			actionRan = true
			return nil
		},
	}

	err := runTransactionalCommand(t, cmd, []string{"foo", "--name", "bad"})
	r.Error(err)
	r.Equal("", dest)
	r.False(actionRan)
}

func TestFlagTransactionDestinationCommittedOnSuccess(t *testing.T) {
	r := require.New(t)

	var dest []string
	cmd := &Command{
		Name: "foo",
		Flags: []Flag{
			&StringSliceFlag{
				Name:        "tags",
				Merge:       MergeStrategyAppend,
				Destination: &dest,
			},
		},
	}

	r.NoError(runTransactionalCommand(t, cmd, []string{"foo", "--tags", "a", "--tags", "b"}))
	r.Equal([]string{"a", "b"}, dest)
}

func TestFlagTransactionOnlyOnceStillEnforced(t *testing.T) {
	r := require.New(t)

	cmd := &Command{
		Name: "foo",
		Flags: []Flag{
			&StringFlag{
				Name:     "name",
				Merge:    MergeStrategyOverride,
				OnlyOnce: true,
			},
		},
	}

	err := runTransactionalCommand(t, cmd, []string{"foo", "--name", "a", "--name", "b"})
	r.Error(err)
	r.Contains(err.Error(), "can't duplicate")
}

func TestFlagTransactionInheritedAcrossParentChild(t *testing.T) {
	r := require.New(t)

	var subTags []string
	root := &Command{
		Name: "app",
		Flags: []Flag{
			&StringSliceFlag{
				Name:  "tags", // persistent: Local is false
				Merge: MergeStrategyAppend,
			},
		},
		Commands: []*Command{
			{
				Name: "sub",
				Action: func(_ context.Context, c *Command) error {
					subTags = c.StringSlice("tags")
					return nil
				},
			},
		},
	}

	r.NoError(runTransactionalCommand(t, root, []string{
		"app", "--tags", "parent", "sub", "--tags", "child",
	}))
	r.Equal([]string{"parent", "child"}, subTags)
}

func TestFlagTransactionInheritedOverride(t *testing.T) {
	r := require.New(t)

	var subName string
	t.Setenv("TXN_NAME", "env-val")

	root := &Command{
		Name: "app",
		Flags: []Flag{
			&StringFlag{
				Name:    "name",
				Sources: EnvVars("TXN_NAME"),
				Merge:   MergeStrategyOverride,
			},
		},
		Commands: []*Command{
			{
				Name: "sub",
				Action: func(_ context.Context, c *Command) error {
					subName = c.String("name")
					return nil
				},
			},
		},
	}

	r.NoError(runTransactionalCommand(t, root, []string{"app", "sub", "--name", "cli"}))
	r.Equal("cli", subName)
}

func TestFlagTransactionInheritedSeparatorPerOccurrence(t *testing.T) {
	r := require.New(t)

	// The persistent flag receives one occurrence parsed by the root
	// command (":" separator) and one by the subcommand ("," separator).
	var subTags []string
	root := &Command{
		Name:                      "app",
		SliceFlagSeparator:        ":",
		DisableSliceFlagSeparator: false,
		Flags: []Flag{
			&StringSliceFlag{
				Name:  "tags",
				Merge: MergeStrategyAppend,
			},
		},
		Commands: []*Command{
			{
				Name:               "sub",
				SliceFlagSeparator: ",",
				Action: func(_ context.Context, c *Command) error {
					subTags = c.StringSlice("tags")
					return nil
				},
			},
		},
	}

	r.NoError(runTransactionalCommand(t, root, []string{
		"app", "--tags", "a:b", "sub", "--tags", "c,d",
	}))
	r.Equal([]string{"a", "b", "c", "d"}, subTags)
}

func TestFlagTransactionResetOnReusedCommand(t *testing.T) {
	r := require.New(t)

	cmd := &Command{
		Name: "foo",
		Flags: []Flag{
			&IntFlag{
				Name:  "n",
				Merge: MergeStrategyOverride,
			},
		},
	}

	r.NoError(runTransactionalCommand(t, cmd, []string{"foo", "--n", "1", "--n", "2"}))
	r.Equal(2, cmd.Int("n"))
	r.Equal(2, cmd.Count("n"))

	// A second run must not see buffered values from the first run.
	r.NoError(runTransactionalCommand(t, cmd, []string{"foo", "--n", "3"}))
	r.Equal(3, cmd.Int("n"))
	r.Equal(1, cmd.Count("n"))
}

func TestFlagTransactionMapAppendAndOverride(t *testing.T) {
	t.Run("append overlays entries", func(t *testing.T) {
		r := require.New(t)

		cmd := &Command{
			Name: "foo",
			Flags: []Flag{
				&StringMapFlag{
					Name:  "props",
					Merge: MergeStrategyAppend,
				},
			},
		}

		err := runTransactionalCommand(t, cmd, []string{
			"foo", "--props", "a=1,b=2", "--props", "a=3,c=4",
		})
		r.NoError(err)
		r.Equal(map[string]string{"a": "3", "b": "2", "c": "4"}, cmd.Value("props"))
	})

	t.Run("override keeps the last occurrence", func(t *testing.T) {
		r := require.New(t)

		cmd := &Command{
			Name: "foo",
			Flags: []Flag{
				&StringMapFlag{
					Name:  "props",
					Merge: MergeStrategyOverride,
				},
			},
		}

		err := runTransactionalCommand(t, cmd, []string{
			"foo", "--props", "a=1,b=2", "--props", "c=3",
		})
		r.NoError(err)
		r.Equal(map[string]string{"c": "3"}, cmd.Value("props"))
	})
}

func TestFlagTransactionGenericOverride(t *testing.T) {
	r := require.New(t)

	actionRan := false
	cmd := &Command{
		Name: "foo",
		Flags: []Flag{
			&GenericFlag{
				Name:  "pair",
				Value: &Parser{},
				Merge: MergeStrategyOverride,
			},
		},
		Action: func(_ context.Context, c *Command) error {
			actionRan = true
			v := c.Generic("pair")
			r.Equal("second,two", v.String())
			return nil
		},
	}

	r.NoError(runTransactionalCommand(t, cmd, []string{
		"foo", "--pair", "first,one", "--pair", "second,two",
	}))
	r.True(actionRan)
}

func TestFlagTransactionGenericDecodeFailureIsAtomic(t *testing.T) {
	r := require.New(t)

	actionRan := false
	cmd := &Command{
		Name: "foo",
		Flags: []Flag{
			&GenericFlag{
				Name:  "pair",
				Value: &Parser{},
				Merge: MergeStrategyAppend, // scalars fall back to last-wins
			},
		},
		Action: func(context.Context, *Command) error {
			actionRan = true
			return nil
		},
	}

	err := runTransactionalCommand(t, cmd, []string{
		"foo", "--pair", "first,one", "--pair", "bad",
	})
	r.Error(err)
	r.False(actionRan)
}

func TestFlagTransactionBeforeSeesCommittedValue(t *testing.T) {
	r := require.New(t)

	var seen string
	cmd := &Command{
		Name: "foo",
		Flags: []Flag{
			&StringFlag{
				Name:  "name",
				Merge: MergeStrategyOverride,
			},
		},
		Before: func(_ context.Context, c *Command) (context.Context, error) {
			seen = c.String("name")
			return nil, nil
		},
	}

	r.NoError(runTransactionalCommand(t, cmd, []string{"foo", "--name", "a", "--name", "b"}))
	r.Equal("b", seen)
}

func TestFlagTransactionDisabledKeepsEagerBehavior(t *testing.T) {
	r := require.New(t)

	// A repeated scalar flag without Merge keeps historical behavior:
	// eager parsing with the last value winning.
	var seenDuringParsing string
	cmd := &Command{
		Name: "foo",
		Flags: []Flag{
			&StringFlag{
				Name: "name",
				Action: func(_ context.Context, _ *Command, v string) error {
					seenDuringParsing = v
					return nil
				},
			},
			&StringSliceFlag{
				Name: "tags",
			},
		},
	}

	r.NoError(runTransactionalCommand(t, cmd, []string{
		"foo", "--name", "a", "--name", "b", "--tags", "x", "--tags", "y",
	}))
	r.Equal("b", cmd.String("name"))
	r.Equal("b", seenDuringParsing)
	r.Equal([]string{"x", "y"}, cmd.StringSlice("tags"))

	fl := cmd.Flags[0].(*StringFlag)
	r.Nil(fl.txn)
}

func TestFlagTransactionHelpShowsMergeSemantics(t *testing.T) {
	r := require.New(t)

	out := &bytes.Buffer{}
	cmd := &Command{
		Name:   "foo",
		Writer: out,
		Flags: []Flag{
			&StringFlag{
				Name:  "name",
				Usage: "pick a name",
				Merge: MergeStrategyOverride,
			},
			&StringSliceFlag{
				Name:  "tags",
				Usage: "tag the thing",
				Merge: MergeStrategyDedup,
			},
		},
	}

	r.NoError(cmd.Run(buildTestContext(t), []string{"foo", "--help"}))
	help := out.String()
	r.Contains(help, "last value overrides")
	r.Contains(help, "duplicates are ignored")
	r.Contains(help, "[ --tags string ]")
}

func TestFlagTransactionCompletionShowsMergeSemantics(t *testing.T) {
	r := require.New(t)

	fl := &StringSliceFlag{
		Name:  "tags",
		Usage: "tag the thing",
		Merge: MergeStrategyAppend,
	}

	out := &bytes.Buffer{}
	printFlagSuggestions("--", []Flag{fl}, out)

	r.Contains(out.String(), "values are appended")
	r.Contains(out.String(), "--tags")
}
