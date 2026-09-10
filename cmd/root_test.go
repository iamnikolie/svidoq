package cmd

import (
	"testing"
	"time"

	"github.com/iamnikolie/svidoq/internal/config"
	"github.com/stretchr/testify/assert"
)

func TestAuthExempt(t *testing.T) {
	// Reachable before a profile exists.
	for _, name := range []string{
		"svidoq", "version", "skill", "help", "completion", "init", "profiles",
		"bash", "zsh", "fish", "powershell",
	} {
		assert.True(t, authExempt(name), "%s must run without a profile", name)
	}

	// Everything that reaches a database must not be exempt.
	for _, name := range []string{"query", "explain", "tables", "describe", "schemas", "envs", "check", "show"} {
		assert.False(t, authExempt(name), "%s must require a profile", name)
	}
}

func TestRowLimitPrefersFlagAndClampsAtMax(t *testing.T) {
	target := &config.Target{Env: "prod", Limit: 200, MaxLimit: 1000}

	t.Cleanup(func() { limitFlag = 0 })

	limitFlag = 0
	got, warn := rowLimit(target)
	assert.Equal(t, 200, got, "no flag means the environment's limit")
	assert.Empty(t, warn)

	limitFlag = 50
	got, warn = rowLimit(target)
	assert.Equal(t, 50, got)
	assert.Empty(t, warn)

	// Clamped, not refused: failing a query after it already ran helps nobody.
	limitFlag = 99999
	got, warn = rowLimit(target)
	assert.Equal(t, 1000, got)
	assert.Contains(t, warn, "capped at 1000")
}

func TestQueryTimeoutPrefersFlag(t *testing.T) {
	target := &config.Target{Timeout: 30 * time.Second}

	t.Cleanup(func() { timeoutFlag = 0 })

	assert.Equal(t, 30*time.Second, queryTimeout(target))

	timeoutFlag = 2 * time.Second
	assert.Equal(t, 2*time.Second, queryTimeout(target))
}
