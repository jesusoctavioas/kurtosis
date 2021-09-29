/*
 * Copyright (c) 2021 - present Kurtosis Technologies Inc.
 * All Rights Reserved.
 */

package root

import (
	"github.com/kurtosis-tech/kurtosis/cli/commands/sandbox"
	"github.com/kurtosis-tech/kurtosis/cli/commands/test"
	"github.com/spf13/cobra"
)

var RootCmd = &cobra.Command{
	// Leaving out the "use" will auto-use os.Args[0]
	Use:                        "",
	Short: "A CLI for interacting with the Kurtosis engine",

	// Cobra will print usage whenever any error occurs
	SilenceUsage: true,
}

func init() {
	RootCmd.AddCommand(sandbox.SandboxCmd)
	RootCmd.AddCommand(test.TestCmd)
}
