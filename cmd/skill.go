package cmd

import (
	_ "embed"
	"fmt"

	"github.com/spf13/cobra"
)

//go:embed skill.md
var skillDoc string

var skillCmd = &cobra.Command{
	Use:   "skill",
	Short: "Print the agent reference for this CLI",
	Long:  "Prints the full svidoq reference — commands, flags, and the safety model — for use as agent context.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Fprint(stdout, skillDoc)

		return nil
	},
}

func init() {
	rootCmd.AddCommand(skillCmd)
}
