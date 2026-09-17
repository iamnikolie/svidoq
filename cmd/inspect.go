package cmd

import (
	"fmt"
	"strings"

	"github.com/iamnikolie/svidoq/internal/config"
	"github.com/iamnikolie/svidoq/internal/datasource"
	"github.com/iamnikolie/svidoq/internal/registry"
	"github.com/spf13/cobra"
)

var profilesCmd = &cobra.Command{
	Use:   "profiles",
	Short: "List the configured profiles (workspaces)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		names, err := config.Profiles()
		if err != nil {
			return err
		}

		if len(names) == 0 {
			home, _ := config.Home()

			return fmt.Errorf("no profiles in %s — run 'svidoq config init --config <name>'", home)
		}

		for _, n := range names {
			fmt.Fprintln(stdout, n)
		}

		return nil
	},
}

var envsCmd = &cobra.Command{
	Use:   "envs",
	Short: "List the environments of the profile",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		names := cfg.EnvNames()
		if len(names) == 0 {
			return fmt.Errorf("no environments in %s", cfg.SourcePath())
		}

		def := cfg.ResolveEnv("")

		for _, n := range names {
			marker := ""
			if n == def {
				marker = "  (default)"
			}

			fmt.Fprintf(stdout, "%s%s\n", n, marker)
		}

		return nil
	},
}

var schemasCmd = &cobra.Command{
	Use:   "schemas",
	Short: "List the schemas reachable in an environment",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		names, discoverErr, err := schemaNames(cmd)
		if err != nil {
			return err
		}

		if discoverErr != nil {
			fmt.Fprintf(stderr, "warning: %v — listing configured schemas only\n", discoverErr)
		}

		if len(names) == 0 {
			return fmt.Errorf("no schemas in %s", cfg.SourcePath())
		}

		rows := make([]map[string]any, 0, len(names))

		for _, n := range names {
			t, resolveErr := cfg.Resolve(envFlag, n)
			if resolveErr != nil {
				rows = append(rows, map[string]any{"schema": n, "target": "!! " + resolveErr.Error(), "description": ""})

				continue
			}

			rows = append(rows, map[string]any{"schema": n, "target": t.Display, "description": t.Description})
		}

		return emit(&datasource.Result{
			Env:      cfg.ResolveEnv(envFlag),
			Columns:  []string{"schema", "description", "target"},
			Rows:     rows,
			RowCount: len(rows),
		})
	},
}

var checkCmd = &cobra.Command{
	Use:   "check",
	Short: "Connect to every schema in the environment and report what answers",
	Long: `Connect to every schema in the environment and report what answers.

Use it after editing a config, or as the first thing in triage: it separates
"the query is wrong" from "this database is unreachable from here".`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		names, discoverErr, err := schemaNames(cmd)
		if err != nil {
			return err
		}

		rows := make([]map[string]any, 0, len(names)+1)
		failed := 0

		if discoverErr != nil {
			failed++

			rows = append(rows, map[string]any{"schema": "(discover)", "status": "unreachable", "detail": firstLine(discoverErr.Error())})
		}

		for _, n := range names {
			status, detail := probe(cmd, n)
			if status != "ok" {
				failed++
			}

			rows = append(rows, map[string]any{"schema": n, "status": status, "detail": detail})
		}

		if err := emit(&datasource.Result{
			Env:      cfg.ResolveEnv(envFlag),
			Columns:  []string{"schema", "status", "detail"},
			Rows:     rows,
			RowCount: len(rows),
		}); err != nil {
			return err
		}

		if failed > 0 {
			return fmt.Errorf("%d of %d schemas unreachable", failed, len(rows))
		}

		return nil
	},
}

// discoverLimit caps SHOW DATABASES. A server with more databases than this is
// not one a person browses by name, and a truncated list must not pass silently.
const discoverLimit = 10000

// schemaNames lists an environment's schemas: the configured ones plus, when
// discover is on, every database the server shows. A failed discovery is
// returned separately so the configured names stay usable.
func schemaNames(cmd *cobra.Command) (names []string, discoverErr, err error) {
	names, err = cfg.SchemaNames(envFlag)
	if err != nil || !cfg.Discovers(envFlag) {
		return names, nil, err
	}

	databases, discoverErr := discover(cmd)
	if discoverErr != nil {
		return names, fmt.Errorf("discovery failed: %w", discoverErr), nil
	}

	names, err = cfg.WithDiscovered(envFlag, databases)

	return names, nil, err
}

// discover asks the environment's server which databases this user can see.
func discover(cmd *cobra.Command) ([]string, error) {
	ds, target, err := registry.OpenServer(cfg, envFlag)
	if err != nil {
		return nil, err
	}
	defer ds.Close()

	res, err := ds.Query(cmd.Context(), "SHOW DATABASES",
		datasource.QueryOpts{Limit: discoverLimit, Timeout: queryTimeout(target)})
	if err != nil {
		return nil, err
	}

	if res.Truncated {
		return nil, fmt.Errorf("SHOW DATABASES returned more than %d databases", discoverLimit)
	}

	if len(res.Columns) == 0 {
		return nil, fmt.Errorf("SHOW DATABASES returned no columns")
	}

	out := make([]string, 0, len(res.Rows))
	for _, row := range res.Rows {
		out = append(out, fmt.Sprint(row[res.Columns[0]]))
	}

	return out, nil
}

// probe opens one schema and asks the server who it thinks we are. It returns
// a status word and a one-line detail rather than an error, so one dead schema
// does not hide the state of the others.
func probe(cmd *cobra.Command, schema string) (status, detail string) {
	ds, target, err := registry.Open(cfg, envFlag, schema)
	if err != nil {
		return "config", firstLine(err.Error())
	}
	defer ds.Close()

	res, err := ds.Query(cmd.Context(), "SELECT CURRENT_USER() AS user, DATABASE() AS db, VERSION() AS version",
		datasource.QueryOpts{Limit: 1, Timeout: queryTimeout(target)})
	if err != nil {
		return "unreachable", firstLine(err.Error())
	}

	if len(res.Rows) == 0 {
		return "unreachable", "no rows from identity query"
	}

	row := res.Rows[0]

	return "ok", fmt.Sprintf("%v @ %v (%v)", row["user"], row["db"], row["version"])
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}

	return s
}

func init() {
	rootCmd.AddCommand(profilesCmd, envsCmd, schemasCmd, checkCmd)
}
