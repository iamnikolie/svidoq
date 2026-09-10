// Package cmd implements the svidoq command line.
package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/iamnikolie/svidoq/internal/config"
	"github.com/spf13/cobra"
)

var (
	profileFlag string
	envFlag     string
	jsonOutput  bool
	formatFlag  string
	limitFlag   int
	timeoutFlag time.Duration
	quiet       bool

	cfg *config.Config
)

// stdout and stderr are indirected so tests can capture them.
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)

var rootCmd = &cobra.Command{
	Use:   "svidoq",
	Short: "Read-only SQL for agents — a witness, not a hand",
	Long: `svidoq — read-only SQL for agents.

A witness sees everything and changes nothing. Every statement is parsed and
allowlisted before it is sent, and then runs inside a read-only transaction, so
a write cannot reach the database through this tool. Results are capped and
timed so a careless query cannot flood a context window or pin a server.

One profile is one workspace (a job, a client). Inside it, an environment is a
server and a schema is a database on that server:

  svidoq --config acme --env prod query billing "SELECT id FROM invoices LIMIT 5"

Run 'svidoq skill' for the full agent reference.`,
	SilenceUsage: true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if authExempt(cmd.Name()) {
			return nil
		}

		if profileFlag == "" {
			profileFlag = os.Getenv("SVIDOQ_CONFIG")
		}

		if profileFlag == "" {
			return fmt.Errorf("--config <profile> is required (or set SVIDOQ_CONFIG); "+
				"there is no default profile so a query cannot reach the wrong workspace by accident%s",
				knownProfilesHint())
		}

		var err error

		cfg, err = config.Load(profileFlag)
		if err != nil {
			return err
		}

		return nil
	},
}

// authExempt lists the commands that run without a profile: everything a user
// reaches before they have one configured, plus cobra's own help and
// completion machinery, whose generated subcommands are named after the shell.
func authExempt(name string) bool {
	switch name {
	case "svidoq", "version", "skill", "help", "completion", "init", "profiles",
		"bash", "zsh", "fish", "powershell":
		return true
	}

	return false
}

func knownProfilesHint() string {
	names, err := config.Profiles()
	if err != nil || len(names) == 0 {
		return "; run 'svidoq config init --config <name>' to create one"
	}

	return "; configured: " + strings.Join(names, ", ")
}

// announce prints the fully resolved target to stderr before a query runs.
// Stdout stays clean for piping, and an agent can never be in doubt about
// which workspace, environment and database it just read.
func announce(t *config.Target) {
	if quiet {
		return
	}

	fmt.Fprintf(stderr, "→ %s/%s/%s  %s\n", t.Profile, t.Env, t.Schema, t.Display)
}

// queryTimeout returns the per-query deadline: the flag when given, else the
// environment's configured value.
func queryTimeout(t *config.Target) time.Duration {
	if timeoutFlag > 0 {
		return timeoutFlag
	}

	return t.Timeout
}

// rowLimit returns the row cap: the flag when given, else the environment's
// default. A flag above the environment's max_limit is clamped rather than
// rejected, because failing a long query after it ran helps nobody.
func rowLimit(t *config.Target) (int, string) {
	limit := t.Limit

	if limitFlag > 0 {
		limit = limitFlag
	}

	if limit > t.MaxLimit {
		return t.MaxLimit, fmt.Sprintf("limit %d exceeds max_limit for %s; capped at %d",
			limit, t.Env, t.MaxLimit)
	}

	return limit, ""
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.ExecuteContext(context.Background()); err != nil {
		os.Exit(1)
	}
}

func init() {
	pf := rootCmd.PersistentFlags()
	pf.StringVar(&profileFlag, "config", "", "profile (workspace) to use; or SVIDOQ_CONFIG")
	pf.StringVar(&envFlag, "env", "", "environment within the profile (default: its default_env)")
	pf.BoolVar(&jsonOutput, "json", false, "alias for --format json")
	pf.StringVar(&formatFlag, "format", "table", "output format: table|json|csv|tsv")
	pf.IntVar(&limitFlag, "limit", 0, "max rows to return (default: the environment's limit)")
	pf.DurationVar(&timeoutFlag, "timeout", 0, "per-query timeout (default: the environment's timeout)")
	pf.BoolVar(&quiet, "quiet", false, "do not print the resolved target to stderr")
}
