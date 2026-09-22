package command_test

import (
	"testing"

	"github.com/cirruslabs/tart-guest-agent/internal/command"
	"github.com/stretchr/testify/require"
)

func TestExecWrapperFlagPreservesArguments(t *testing.T) {
	cmd := command.NewRootCommand()
	require.NoError(t, cmd.ParseFlags([]string{
		"--run-rpc",
		"--exec-wrapper=/usr/bin/env",
		"--exec-wrapper=--",
		"--exec-wrapper=NAME=value,with spaces",
		"--exec-wrapper=",
		"--exec-wrapper=literal $HOME; $(false)",
	}))
	argv, err := cmd.Flags().GetStringArray("exec-wrapper")
	require.NoError(t, err)
	require.Equal(t, []string{
		"/usr/bin/env", "--", "NAME=value,with spaces", "", "literal $HOME; $(false)",
	}, argv)

	defaultCommand := command.NewRootCommand()
	argv, err = defaultCommand.Flags().GetStringArray("exec-wrapper")
	require.NoError(t, err)
	require.Empty(t, argv)
}
