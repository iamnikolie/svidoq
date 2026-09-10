package cmd

import (
	"github.com/iamnikolie/svidoq/internal/config"
	"github.com/iamnikolie/svidoq/internal/datasource"
	"github.com/spf13/cobra"
)

var tableFilter string

var tablesCmd = &cobra.Command{
	Use:     "tables <schema>",
	Short:   "List the tables of a schema",
	Args:    cobra.ExactArgs(1),
	Example: `  svidoq --config acme --env prod tables billing --filter "invoice%"`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runOn(cmd, args[0], func(ds datasource.Datasource, opts datasource.QueryOpts) (*datasource.Result, error) {
			return ds.Tables(cmd.Context(), tableFilter, metaOpts(opts))
		})
	},
}

var describeCmd = &cobra.Command{
	Use:     "describe <schema> <table>",
	Short:   "Show the columns of a table",
	Args:    cobra.ExactArgs(2),
	Example: `  svidoq --config acme --env prod describe billing invoices`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runOn(cmd, args[0], func(ds datasource.Datasource, opts datasource.QueryOpts) (*datasource.Result, error) {
			return ds.Describe(cmd.Context(), args[1], metaOpts(opts))
		})
	},
}

// metaOpts raises the row cap for schema metadata. A 200-row default is right
// for data, but a truncated table list is a trap: the table you needed is the
// one that was cut off.
func metaOpts(opts datasource.QueryOpts) datasource.QueryOpts {
	if opts.Limit < config.DefaultMaxLimit {
		opts.Limit = config.DefaultMaxLimit
	}

	return opts
}

func init() {
	tablesCmd.Flags().StringVar(&tableFilter, "filter", "", "SQL LIKE pattern on the table name")
	rootCmd.AddCommand(tablesCmd, describeCmd)
}
