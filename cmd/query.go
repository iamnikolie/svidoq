package cmd

import (
	"fmt"

	"github.com/iamnikolie/svidoq/internal/datasource"
	"github.com/iamnikolie/svidoq/internal/registry"
	"github.com/spf13/cobra"
)

var queryCmd = &cobra.Command{
	Use:   "query <schema> <sql>",
	Short: "Run one read-only statement",
	Long: `Run one read-only statement against a schema.

Only a single SELECT, UNION, SHOW, DESCRIBE or EXPLAIN is accepted, checked by
parsing the statement rather than by matching its text, and it runs inside a
read-only transaction.`,
	Example: `  svidoq --config acme --env prod query billing "SELECT id, total FROM invoices WHERE id = 42"
  svidoq --config acme query billing "SELECT * FROM invoices" --limit 20 --json
  svidoq --config acme query billing "SHOW CREATE TABLE invoices"`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runOn(cmd, args[0], func(ds datasource.Datasource, opts datasource.QueryOpts) (*datasource.Result, error) {
			return ds.Query(cmd.Context(), args[1], opts)
		})
	},
}

var explainCmd = &cobra.Command{
	Use:     "explain <schema> <sql>",
	Short:   "Show the query plan for a read-only statement",
	Args:    cobra.ExactArgs(2),
	Example: `  svidoq --config acme --env prod explain billing "SELECT * FROM invoices WHERE client_id = 7"`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runOn(cmd, args[0], func(ds datasource.Datasource, opts datasource.QueryOpts) (*datasource.Result, error) {
			return ds.Explain(cmd.Context(), args[1], opts)
		})
	},
}

// runOn resolves a schema, announces the target, runs one operation and emits
// the result. Every data command funnels through it so the announcement, the
// limits and the cleanup cannot drift apart between commands.
func runOn(cmd *cobra.Command, schema string, op func(datasource.Datasource, datasource.QueryOpts) (*datasource.Result, error)) error {
	ds, target, err := registry.Open(cfg, envFlag, schema)
	if err != nil {
		return err
	}
	defer ds.Close()

	announce(target)

	limit, warn := rowLimit(target)
	if warn != "" {
		fmt.Fprintln(stderr, warn)
	}

	res, err := op(ds, datasource.QueryOpts{Limit: limit, Timeout: queryTimeout(target)})
	if err != nil {
		return err
	}

	res.Env = target.Env

	return emit(res)
}

func init() {
	rootCmd.AddCommand(queryCmd, explainCmd)
}
