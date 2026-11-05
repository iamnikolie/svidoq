package registry

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/svidoq/internal/config"
	"github.com/iamnikolie/svidoq/internal/datasource"
	"github.com/iamnikolie/svidoq/internal/tunnel"
	"github.com/iamnikolie/svidoq/internal/tunnel/sshtest"
)

// An environment with an ssh block reaches its host:port from the SSH host.
// The fake server permits no forward, so the query fails — at the tunnel, for
// the database's address as configured, which is the wiring under test.
func TestSSHEnvironmentDialsThroughTheTunnel(t *testing.T) {
	f := sshtest.New(t)

	home := t.TempDir()
	t.Setenv("SVIDOQ_HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, "acme"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, "acme", "config.yaml"), fmt.Appendf(nil, `
default_env: dev
environments:
  dev:
    host: 127.0.0.1
    port: 3306
    user: readonly
    password: pw
    ssh:
      host: %s
      port: %d
      user: %s
      key: %s
      known_hosts: %s
schemas:
  billing: {}
`, f.Config.Host, f.Config.Port, f.Config.User, f.Config.Key, f.Config.KnownHosts), 0o600))

	cfg, err := config.Load("acme")
	require.NoError(t, err)

	ds, target, err := Open(cfg, "", "billing")
	require.NoError(t, err)

	assert.Contains(t, target.Display, "via ssh reader@")

	_, err = ds.Query(context.Background(), "SELECT 1", datasource.QueryOpts{Limit: 1, Timeout: 5 * time.Second})
	require.Error(t, err)
	assert.Contains(t, err.Error(), tunnel.ErrTunnel.Error())
	assert.Contains(t, err.Error(), "open 127.0.0.1:3306 via reader@")

	stats := f.Server.Stats()
	assert.Equal(t, 1, stats.Handshakes)
	assert.Contains(t, stats.Channels, "direct-tcpip 127.0.0.1:3306")

	require.NoError(t, ds.Close())
}
