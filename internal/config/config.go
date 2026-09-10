// Package config loads a svidoq profile.
//
// A profile is a directory under ~/.svidoq (override with SVIDOQ_HOME) holding
// one config.yaml, exactly like the sibling CLIs' ~/.gl/<profile>/ and
// ~/.slk/<profile>/. One profile is one workspace: a job, a client, a personal
// machine. Nothing about a workspace lives in this repository.
//
// Inside a profile, an environment describes a *server* and a schema describes
// a *database on it*, so six schemas across three environments are nine config
// entries rather than eighteen. Field resolution runs innermost-first:
// per-environment schema override, then the top-level schema, then the
// environment, then the built-in default.
package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Built-in defaults. Limit is deliberately small: the common failure mode of a
// database in an agent loop is not a dropped table, it is fifty thousand rows
// landing in a context window.
const (
	DefaultLimit    = 200
	DefaultMaxLimit = 5000
	DefaultTimeout  = 30 * time.Second
	ConnectTimeout  = 10 * time.Second
	DefaultPort     = 3306
	DefaultDriver   = "mysql"
)

var (
	// ErrNoProfile is returned when no profile was selected.
	ErrNoProfile = errors.New("no profile selected")
	// ErrUnknownEnvironment is returned when a requested environment is absent.
	ErrUnknownEnvironment = errors.New("unknown environment")
	// ErrUnknownSchema is returned when a requested schema is absent.
	ErrUnknownSchema = errors.New("unknown schema")
	// ErrIncomplete marks a target that cannot produce a DSN.
	ErrIncomplete = errors.New("incomplete configuration")
)

// Schema is one database on the environment's server.
type Schema struct {
	Description string `yaml:"description"`

	// Database defaults to the schema's key in the map.
	Database string `yaml:"database"`

	// Connection overrides, for a schema that does not live with the others.
	Host            string `yaml:"host"`
	Port            int    `yaml:"port"`
	User            string `yaml:"user"`
	Password        string `yaml:"password"`
	PasswordCommand string `yaml:"password_command"`
	Params          string `yaml:"params"`

	// URL is a full DSN escape hatch. When set it is used verbatim and every
	// other connection field on this schema is ignored.
	URL string `yaml:"url"`
}

// Environment is a server: one host, one credential, one set of limits.
type Environment struct {
	Driver          string `yaml:"driver"`
	Host            string `yaml:"host"`
	Port            int    `yaml:"port"`
	User            string `yaml:"user"`
	Password        string `yaml:"password"`
	PasswordCommand string `yaml:"password_command"`
	Params          string `yaml:"params"`

	Limit    int           `yaml:"limit"`
	MaxLimit int           `yaml:"max_limit"`
	Timeout  time.Duration `yaml:"timeout"`

	// Schemas overrides top-level schema entries for this environment only.
	Schemas map[string]Schema `yaml:"schemas"`
}

// Config is one profile.
type Config struct {
	DefaultEnv   string                 `yaml:"default_env"`
	Environments map[string]Environment `yaml:"environments"`
	Schemas      map[string]Schema      `yaml:"schemas"`

	profile string
	path    string
}

// Target is a fully resolved connection: everything needed to run one query.
type Target struct {
	Profile     string
	Env         string
	Schema      string
	Description string
	Driver      string
	DSN         string
	Limit       int
	MaxLimit    int
	Timeout     time.Duration

	// Display is the DSN with the password replaced, safe to print.
	Display string
}

// Home returns the base directory holding every profile.
func Home() (string, error) {
	if h := os.Getenv("SVIDOQ_HOME"); h != "" {
		return h, nil
	}

	u, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}

	return filepath.Join(u, ".svidoq"), nil
}

// Dir returns a profile's directory.
func Dir(profile string) (string, error) {
	if profile == "" {
		return "", ErrNoProfile
	}

	base, err := Home()
	if err != nil {
		return "", err
	}

	return filepath.Join(base, profile), nil
}

// Path returns a profile's config file path.
func Path(profile string) (string, error) {
	dir, err := Dir(profile)
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, "config.yaml"), nil
}

// Profiles lists the profiles that exist on disk, sorted.
func Profiles() ([]string, error) {
	base, err := Home()
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("read %s: %w", base, err)
	}

	var out []string

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		if _, statErr := os.Stat(filepath.Join(base, e.Name(), "config.yaml")); statErr == nil {
			out = append(out, e.Name())
		}
	}

	sort.Strings(out)

	return out, nil
}

// Load reads a profile from disk.
func Load(profile string) (*Config, error) {
	path, err := Path(profile)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	cfg.profile = profile
	cfg.path = path

	if cfg.Environments == nil {
		cfg.Environments = map[string]Environment{}
	}

	if cfg.Schemas == nil {
		cfg.Schemas = map[string]Schema{}
	}

	return &cfg, nil
}

// Profile returns the loaded profile name.
func (c *Config) Profile() string { return c.profile }

// SourcePath returns the file this config was read from.
func (c *Config) SourcePath() string { return c.path }

// ResolveEnv returns the effective environment name: the request, else the
// configured default. An empty result means the caller must pass --env.
func (c *Config) ResolveEnv(requested string) string {
	if requested != "" {
		return requested
	}

	return c.DefaultEnv
}

// EnvNames returns configured environment names, sorted.
func (c *Config) EnvNames() []string {
	names := make([]string, 0, len(c.Environments))
	for name := range c.Environments {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// SchemaNames returns the schemas visible in an environment, sorted. It is the
// union of the top-level schemas and any the environment adds.
func (c *Config) SchemaNames(env string) ([]string, error) {
	e, name, err := c.environment(env)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	for n := range c.Schemas {
		seen[n] = true
	}

	for n := range e.Schemas {
		seen[n] = true
	}

	_ = name

	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}

	sort.Strings(out)

	return out, nil
}

func (c *Config) environment(requested string) (Environment, string, error) {
	name := c.ResolveEnv(requested)
	if name == "" {
		return Environment{}, "", fmt.Errorf("%w: no --env given and no default_env in %s (configured: %s)",
			ErrUnknownEnvironment, c.path, strings.Join(c.EnvNames(), ", "))
	}

	e, ok := c.Environments[name]
	if !ok {
		return Environment{}, "", fmt.Errorf("%w: %q (configured: %s)",
			ErrUnknownEnvironment, name, strings.Join(c.EnvNames(), ", "))
	}

	return e, name, nil
}

// Resolve produces the connection target for one schema in one environment.
func (c *Config) Resolve(env, schema string) (*Target, error) {
	e, envName, err := c.environment(env)
	if err != nil {
		return nil, err
	}

	base, hasBase := c.Schemas[schema]
	override, hasOverride := e.Schemas[schema]

	if !hasBase && !hasOverride {
		names, _ := c.SchemaNames(envName)

		return nil, fmt.Errorf("%w: %q in environment %q (configured: %s)",
			ErrUnknownSchema, schema, envName, strings.Join(names, ", "))
	}

	s := mergeSchema(base, override)

	t := &Target{
		Profile:     c.profile,
		Env:         envName,
		Schema:      schema,
		Description: s.Description,
		Driver:      firstNonEmpty(e.Driver, DefaultDriver),
		Limit:       firstPositive(e.Limit, DefaultLimit),
		MaxLimit:    firstPositive(e.MaxLimit, DefaultMaxLimit),
		Timeout:     e.Timeout,
	}

	if t.Timeout <= 0 {
		t.Timeout = DefaultTimeout
	}

	if t.MaxLimit < t.Limit {
		t.MaxLimit = t.Limit
	}

	if s.URL != "" {
		t.DSN = os.ExpandEnv(s.URL)
		t.Display = redactDSN(t.DSN)

		return t, nil
	}

	host := firstNonEmpty(s.Host, e.Host)
	if host == "" {
		return nil, fmt.Errorf("%w: no host for schema %q in environment %q", ErrIncomplete, schema, envName)
	}

	user := firstNonEmpty(s.User, e.User)
	if user == "" {
		return nil, fmt.Errorf("%w: no user for schema %q in environment %q", ErrIncomplete, schema, envName)
	}

	password, err := resolvePassword(s, e, envName)
	if err != nil {
		return nil, err
	}

	database := firstNonEmpty(s.Database, schema)
	port := firstPositive(s.Port, e.Port, DefaultPort)
	params := firstNonEmpty(s.Params, e.Params)

	t.DSN = mysqlDSN(user, password, host, port, database, params)
	t.Display = mysqlDSN(user, "", host, port, database, params)

	return t, nil
}

// resolvePassword prefers, in order: the schema's literal or command, the
// environment's, then SVIDOQ_PASSWORD_<ENV>. Literals go through os.ExpandEnv
// so a config can carry ${VAR} instead of the secret itself.
func resolvePassword(s Schema, e Environment, envName string) (string, error) {
	for _, src := range []struct{ literal, command string }{
		{s.Password, s.PasswordCommand},
		{e.Password, e.PasswordCommand},
	} {
		if src.literal != "" {
			return os.ExpandEnv(src.literal), nil
		}

		if src.command != "" {
			out, err := runPasswordCommand(src.command)
			if err != nil {
				return "", err
			}

			return out, nil
		}
	}

	if v := os.Getenv("SVIDOQ_PASSWORD_" + envVarPart(envName)); v != "" {
		return v, nil
	}

	return "", nil
}

// runPasswordCommand shells out to a secret manager. The command's stdout is
// the password, with trailing whitespace trimmed; stderr is surfaced on
// failure because "op: not signed in" is the answer nine times out of ten.
func runPasswordCommand(command string) (string, error) {
	cmd := exec.Command("sh", "-c", command)

	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("password_command failed: %s: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}

		return "", fmt.Errorf("password_command failed: %w", err)
	}

	return strings.TrimRight(string(out), "\r\n \t"), nil
}

// mergeSchema layers an environment-specific override over the base schema.
func mergeSchema(base, override Schema) Schema {
	out := base
	out.Description = firstNonEmpty(override.Description, base.Description)
	out.Database = firstNonEmpty(override.Database, base.Database)
	out.Host = firstNonEmpty(override.Host, base.Host)
	out.User = firstNonEmpty(override.User, base.User)
	out.Password = firstNonEmpty(override.Password, base.Password)
	out.PasswordCommand = firstNonEmpty(override.PasswordCommand, base.PasswordCommand)
	out.Params = firstNonEmpty(override.Params, base.Params)
	out.URL = firstNonEmpty(override.URL, base.URL)
	out.Port = firstPositive(override.Port, base.Port)

	return out
}

// mysqlDSN builds a go-sql-driver DSN. parseTime is forced on so DATETIME
// columns arrive as time.Time rather than []byte.
func mysqlDSN(user, password, host string, port int, database, params string) string {
	var b strings.Builder

	b.WriteString(user)

	if password != "" {
		b.WriteString(":")
		b.WriteString(password)
	}

	fmt.Fprintf(&b, "@tcp(%s:%d)/%s?parseTime=true", host, port, database)

	if params != "" {
		b.WriteString("&")
		b.WriteString(strings.TrimPrefix(params, "&"))
	}

	return b.String()
}

// redactDSN masks the password in a user:pass@... DSN for display.
func redactDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return dsn
	}

	colon := strings.Index(dsn[:at], ":")
	if colon < 0 {
		return dsn
	}

	return dsn[:colon] + ":***" + dsn[at:]
}

func envVarPart(name string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}

func firstPositive(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}

	return 0
}
