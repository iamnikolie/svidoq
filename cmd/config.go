package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/iamnikolie/svidoq/internal/config"
	"github.com/spf13/cobra"
)

// starterConfig is written by `config init`. It is a working example rather
// than an empty skeleton: the fastest way to learn the three-level model is to
// edit something that already has the shape.
const starterConfig = `# svidoq profile. One profile is one workspace.
#
# An environment describes a server; a schema describes a database on it, so
# adding a schema is one line, not one line per environment.
#
# Never commit this file. It is created 0600 for a reason.

default_env: prod

environments:
  prod:
    host: db.example.internal
    port: 3306
    user: readonly
    # Three ways to supply the password, in order of preference:
    #   password_command: "op read op://work/db-prod/password"   # never on disk
    #   password_env: ACME_PROD_PASSWORD                         # from the environment
    #   password: "literal"                                      # used exactly as written
    password_command: "echo change-me"
    limit: 200        # default rows per query
    max_limit: 5000   # ceiling for --limit
    timeout: 30s

  staging:
    host: db-staging.example.internal
    user: readonly
    password_env: ACME_STAGING_PASSWORD
    limit: 2000
    # discover: true  # every database this user can see is a schema, unlisted

schemas:
  billing:
    description: "invoices, payments"
  analytics:
    description: "event rollups"
    # A schema that does not live with the others overrides just what differs:
    # host: analytics-db.example.internal
`

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Create and inspect profiles",
}

var configInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Write a starter profile",
	Long: `Write a starter profile to ~/.svidoq/<profile>/config.yaml.

The file is a commented, working example — edit it, then run 'svidoq check'.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		profile := profileFlag
		if profile == "" {
			profile = os.Getenv("SVIDOQ_CONFIG")
		}

		if profile == "" {
			return fmt.Errorf("--config <profile> is required: it names the workspace to create")
		}

		dir, err := config.Dir(profile)
		if err != nil {
			return err
		}

		path := filepath.Join(dir, "config.yaml")
		if _, statErr := os.Stat(path); statErr == nil {
			return fmt.Errorf("%s already exists — edit it, or pick another --config name", path)
		}

		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}

		if err := os.WriteFile(path, []byte(starterConfig), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}

		fmt.Fprintf(stdout, "%s\n", path)
		fmt.Fprintf(stderr, "edit it, then run: svidoq --config %s check\n", profile)

		return nil
	},
}

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show the profile as it resolves, with passwords masked",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Fprintf(stdout, "profile:  %s\n", cfg.Profile())
		fmt.Fprintf(stdout, "file:     %s\n", cfg.SourcePath())
		fmt.Fprintf(stdout, "default:  %s\n\n", cfg.ResolveEnv(""))

		for _, env := range cfg.EnvNames() {
			fmt.Fprintf(stdout, "[%s]\n", env)

			if cfg.Discovers(env) {
				fmt.Fprintf(stdout, "  %-20s every database the server shows (run 'schemas' to list)\n", "(discover)")
			}

			names, err := cfg.SchemaNames(env)
			if err != nil {
				return err
			}

			for _, s := range names {
				t, resolveErr := cfg.Resolve(env, s)
				if resolveErr != nil {
					fmt.Fprintf(stdout, "  %-20s !! %v\n", s, resolveErr)

					continue
				}

				fmt.Fprintf(stdout, "  %-20s %s  (limit %d, max %d, timeout %s)\n",
					s, t.Display, t.Limit, t.MaxLimit, t.Timeout)
			}

			fmt.Fprintln(stdout)
		}

		return nil
	},
}

func init() {
	configCmd.AddCommand(configInitCmd, configShowCmd)
	rootCmd.AddCommand(configCmd)
}
