package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// write lays down a profile in a throwaway SVIDOQ_HOME and returns its name.
func write(t *testing.T, profile, body string) string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("SVIDOQ_HOME", home)

	dir := filepath.Join(home, profile)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600))

	return profile
}

const sample = `
default_env: prod
environments:
  prod:
    host: db-prod.internal
    user: readonly
    password: s3cret
    limit: 10
    max_limit: 100
    timeout: 5s
  staging:
    host: db-staging.internal
    user: readonly
    password: stagepw
schemas:
  billing:
    description: "invoices"
  analytics:
    host: analytics.internal
    database: analytics_rollup
`

func TestResolveComposesDSNFromEnvironmentAndSchema(t *testing.T) {
	p := write(t, "acme", sample)

	cfg, err := Load(p)
	require.NoError(t, err)

	target, err := cfg.Resolve("", "billing")
	require.NoError(t, err)

	assert.Equal(t, "prod", target.Env, "empty --env resolves to default_env")
	assert.Equal(t, "readonly:s3cret@tcp(db-prod.internal:3306)/billing?parseTime=true", target.DSN)
	assert.Equal(t, 10, target.Limit)
	assert.Equal(t, 100, target.MaxLimit)
	assert.Equal(t, 5*time.Second, target.Timeout)
}

// The point of the three-level model: a schema that lives elsewhere overrides
// only what differs, and inherits the credential.
func TestSchemaOverridesOnlyWhatItSets(t *testing.T) {
	p := write(t, "acme", sample)

	cfg, err := Load(p)
	require.NoError(t, err)

	target, err := cfg.Resolve("prod", "analytics")
	require.NoError(t, err)

	assert.Equal(t, "readonly:s3cret@tcp(analytics.internal:3306)/analytics_rollup?parseTime=true", target.DSN)
}

func TestSameSchemaFollowsTheEnvironment(t *testing.T) {
	p := write(t, "acme", sample)

	cfg, err := Load(p)
	require.NoError(t, err)

	target, err := cfg.Resolve("staging", "billing")
	require.NoError(t, err)

	assert.Contains(t, target.DSN, "db-staging.internal")
	assert.Equal(t, DefaultLimit, target.Limit, "an environment without limits gets the defaults")
	assert.Equal(t, DefaultTimeout, target.Timeout)
}

func TestDisplayNeverCarriesThePassword(t *testing.T) {
	p := write(t, "acme", sample)

	cfg, err := Load(p)
	require.NoError(t, err)

	target, err := cfg.Resolve("prod", "billing")
	require.NoError(t, err)

	assert.NotContains(t, target.Display, "s3cret")
	assert.Contains(t, target.Display, "db-prod.internal")
}

func TestPasswordFromEnvironmentVariable(t *testing.T) {
	p := write(t, "acme", `
default_env: prod
environments:
  prod:
    host: h
    user: u
    password_env: ACME_PW
schemas:
  billing: {}
`)
	t.Setenv("ACME_PW", "from-env")

	cfg, err := Load(p)
	require.NoError(t, err)

	target, err := cfg.Resolve("", "billing")
	require.NoError(t, err)

	assert.Contains(t, target.DSN, ":from-env@")
}

func TestPasswordEnvPointingAtNothingIsAnError(t *testing.T) {
	p := write(t, "acme", `
default_env: prod
environments:
  prod:
    host: h
    user: u
    password_env: ACME_PW_UNSET
schemas:
  billing: {}
`)

	cfg, err := Load(p)
	require.NoError(t, err)

	_, err = cfg.Resolve("", "billing")
	require.ErrorIs(t, err, ErrIncomplete, "a dangling password_env must fail loudly, not connect without a password")
}

// A literal password is used exactly as written. Expanding it would truncate
// every password containing a dollar sign — an entirely ordinary character in
// a generated password, and the bug that this test exists to prevent.
func TestLiteralPasswordIsNotExpanded(t *testing.T) {
	p := write(t, "acme", `
default_env: prod
environments:
  prod:
    host: h
    user: u
    password: "aA1$bC2dE3f"
schemas:
  billing: {}
`)
	t.Setenv("bC2dE3f", "SHOULD-NOT-APPEAR")

	cfg, err := Load(p)
	require.NoError(t, err)

	target, err := cfg.Resolve("", "billing")
	require.NoError(t, err)

	assert.Contains(t, target.DSN, ":aA1$bC2dE3f@")
	assert.NotContains(t, target.DSN, "SHOULD-NOT-APPEAR")
}

// The same hazard through the full-DSN escape hatch.
func TestURLIsNotExpanded(t *testing.T) {
	p := write(t, "acme", `
default_env: prod
environments:
  prod: {host: h, user: u}
schemas:
  legacy:
    url: "u:pa$$word@tcp(1.2.3.4:3306)/legacy?parseTime=true"
`)

	cfg, err := Load(p)
	require.NoError(t, err)

	target, err := cfg.Resolve("", "legacy")
	require.NoError(t, err)

	assert.Equal(t, "u:pa$$word@tcp(1.2.3.4:3306)/legacy?parseTime=true", target.DSN)
}

func TestPasswordFromCommand(t *testing.T) {
	p := write(t, "acme", `
default_env: prod
environments:
  prod:
    host: h
    user: u
    password_command: "printf 'from-command\n'"
schemas:
  billing: {}
`)

	cfg, err := Load(p)
	require.NoError(t, err)

	target, err := cfg.Resolve("", "billing")
	require.NoError(t, err)

	assert.Contains(t, target.DSN, ":from-command@", "trailing newline must be trimmed")
}

func TestPasswordCommandFailureIsReported(t *testing.T) {
	p := write(t, "acme", `
default_env: prod
environments:
  prod:
    host: h
    user: u
    password_command: "echo 'not signed in' >&2; exit 1"
schemas:
  billing: {}
`)

	cfg, err := Load(p)
	require.NoError(t, err)

	_, err = cfg.Resolve("", "billing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not signed in", "stderr from the secret manager is the useful part")
}

func TestPasswordFromProcessEnvFallback(t *testing.T) {
	p := write(t, "acme", `
default_env: prod
environments:
  prod:
    host: h
    user: u
schemas:
  billing: {}
`)
	t.Setenv("SVIDOQ_PASSWORD_PROD", "fallback")

	cfg, err := Load(p)
	require.NoError(t, err)

	target, err := cfg.Resolve("", "billing")
	require.NoError(t, err)

	assert.Contains(t, target.DSN, ":fallback@")
}

func TestURLEscapeHatchWins(t *testing.T) {
	p := write(t, "acme", `
default_env: prod
environments:
  prod:
    host: ignored
    user: ignored
schemas:
  legacy:
    url: "u:p@tcp(1.2.3.4:3307)/legacy?parseTime=true"
`)

	cfg, err := Load(p)
	require.NoError(t, err)

	target, err := cfg.Resolve("", "legacy")
	require.NoError(t, err)

	assert.Equal(t, "u:p@tcp(1.2.3.4:3307)/legacy?parseTime=true", target.DSN)
	assert.Equal(t, "u:***@tcp(1.2.3.4:3307)/legacy?parseTime=true", target.Display)
}

func TestUnknownEnvironmentAndSchemaListWhatExists(t *testing.T) {
	p := write(t, "acme", sample)

	cfg, err := Load(p)
	require.NoError(t, err)

	_, err = cfg.Resolve("nope", "billing")
	require.ErrorIs(t, err, ErrUnknownEnvironment)
	assert.Contains(t, err.Error(), "prod", "the error should list the configured environments")

	_, err = cfg.Resolve("prod", "nope")
	require.ErrorIs(t, err, ErrUnknownSchema)
	assert.Contains(t, err.Error(), "billing")
}

func TestIncompleteConfigurationIsRejectedBeforeDialing(t *testing.T) {
	p := write(t, "acme", `
default_env: prod
environments:
  prod:
    user: u
schemas:
  billing: {}
`)

	cfg, err := Load(p)
	require.NoError(t, err)

	_, err = cfg.Resolve("", "billing")
	require.ErrorIs(t, err, ErrIncomplete)
}

func TestMissingDefaultEnvIsAnActionableError(t *testing.T) {
	p := write(t, "acme", `
environments:
  prod: {host: h, user: u}
  staging: {host: h2, user: u}
schemas:
  billing: {}
`)

	cfg, err := Load(p)
	require.NoError(t, err)

	_, err = cfg.Resolve("", "billing")
	require.ErrorIs(t, err, ErrUnknownEnvironment)
	assert.Contains(t, err.Error(), "--env")
}

func TestProfilesListsOnlyDirectoriesWithAConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SVIDOQ_HOME", home)

	require.NoError(t, os.MkdirAll(filepath.Join(home, "acme"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, "acme", "config.yaml"), []byte("environments: {}"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(home, "empty"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, "stray.txt"), []byte("x"), 0o600))

	names, err := Profiles()
	require.NoError(t, err)
	assert.Equal(t, []string{"acme"}, names)
}

func TestSchemaNamesUnionsEnvironmentOverrides(t *testing.T) {
	p := write(t, "acme", `
default_env: prod
environments:
  prod:
    host: h
    user: u
    schemas:
      prod_only: {}
schemas:
  billing: {}
`)

	cfg, err := Load(p)
	require.NoError(t, err)

	names, err := cfg.SchemaNames("prod")
	require.NoError(t, err)
	assert.Equal(t, []string{"billing", "prod_only"}, names)
}

func TestNoProfileIsNotAPathTraversal(t *testing.T) {
	_, err := Dir("")
	require.ErrorIs(t, err, ErrNoProfile)
}
