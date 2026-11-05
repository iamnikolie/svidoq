package tunnel

import (
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/iamnikolie/svidoq/internal/config"
	"github.com/iamnikolie/svidoq/internal/tunnel/sshtest"
)

// fixture is a working server, key and known_hosts with an echo server as the
// database; tests break one piece at a time.
type fixture struct {
	sshtest.Fixture

	echo string
}

func newFixture(t *testing.T) fixture {
	t.Helper()

	t.Setenv(PassphraseEnv, "")

	echo := sshtest.Echo(t)

	return fixture{Fixture: sshtest.New(t, echo), echo: echo}
}

func roundTrip(t *testing.T, c net.Conn, msg string) {
	t.Helper()

	_, err := c.Write([]byte(msg))
	require.NoError(t, err)

	buf := make([]byte, len(msg))
	_, err = io.ReadFull(c, buf)
	require.NoError(t, err)
	assert.Equal(t, msg, string(buf))
}

func TestForwardReachesTheAddressAsSeenFromTheSSHHost(t *testing.T) {
	f := newFixture(t)

	tun := New(f.Config, 5*time.Second)
	defer tun.Close()

	c, err := tun.DialContext(context.Background(), "tcp", f.echo)
	require.NoError(t, err)
	roundTrip(t, c, "SELECT 1")
	require.NoError(t, c.Close())

	channels := f.Server.Stats().Channels
	assert.Equal(t, []string{"direct-tcpip " + f.echo}, channels)
}

func TestOneSSHConnectionCarriesEveryForwardUntilClose(t *testing.T) {
	f := newFixture(t)

	tun := New(f.Config, 5*time.Second)

	for range 3 {
		c, err := tun.DialContext(context.Background(), "tcp", f.echo)
		require.NoError(t, err)
		roundTrip(t, c, "ping")
		_ = c.Close()
	}

	assert.Equal(t, 1, f.Server.Stats().Handshakes, "the pool and KILL QUERY share one SSH connection")

	open, err := tun.DialContext(context.Background(), "tcp", f.echo)
	require.NoError(t, err)

	require.NoError(t, tun.Close())

	// Closing the tunnel ends the forwards riding on it.
	_ = open.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = open.Read(make([]byte, 1))
	assert.Error(t, err)

	_, err = tun.DialContext(context.Background(), "tcp", f.echo)
	require.ErrorIs(t, err, ErrTunnel)
	assert.Contains(t, err.Error(), "closed")
	assert.NoError(t, tun.Close(), "Close is idempotent")
}

func TestOnlyForwardsAreRequested(t *testing.T) {
	f := newFixture(t)

	tun := New(f.Config, 5*time.Second)

	c, err := tun.DialContext(context.Background(), "tcp", f.echo)
	require.NoError(t, err)
	roundTrip(t, c, "x")
	_ = c.Close()
	require.NoError(t, tun.Close())

	// No session channel means no shell, no command and no agent forwarding,
	// which OpenSSH can only request on a session.
	stats := f.Server.Stats()
	for _, ch := range stats.Channels {
		assert.True(t, strings.HasPrefix(ch, "direct-tcpip "), "unexpected channel %q", ch)
	}

	assert.Empty(t, stats.Requests, "no global or channel requests")
}

func TestRefusedForwardIsReported(t *testing.T) {
	f := newFixture(t)

	tun := New(f.Config, 5*time.Second)
	defer tun.Close()

	// The fake server permits only the echo address, as PermitOpen would.
	_, err := tun.DialContext(context.Background(), "tcp", "127.0.0.1:1")
	require.ErrorIs(t, err, ErrTunnel)
	assert.Contains(t, err.Error(), "refused the forward")
	assert.Contains(t, err.Error(), "prohibited")
}

func TestUnknownHostIsRefusedAndNotLearned(t *testing.T) {
	f := newFixture(t)

	other, _ := sshtest.Ed25519(t)
	f.Config.KnownHosts = sshtest.WriteKnownHosts(t, "203.0.113.7:22", other.PublicKey())

	tun := New(f.Config, 5*time.Second)
	defer tun.Close()

	_, err := tun.DialContext(context.Background(), "tcp", f.echo)
	require.ErrorIs(t, err, ErrTunnel)
	assert.Contains(t, err.Error(), "is not in")
	assert.Contains(t, err.Error(), "SHA256:", "the fingerprint is shown so it can be verified")

	assert.Zero(t, f.Server.Stats().AuthTries, "no credential is offered to an unverified host")

	_, err = New(f.Config, 5*time.Second).DialContext(context.Background(), "tcp", f.echo)
	require.Error(t, err, "a refused key is not remembered as accepted")
}

func TestChangedHostKeyIsRefused(t *testing.T) {
	f := newFixture(t)

	impostor, _ := sshtest.Ed25519(t)
	f.Config.KnownHosts = sshtest.WriteKnownHosts(t, f.Server.Addr, impostor.PublicKey())

	_, err := New(f.Config, 5*time.Second).DialContext(context.Background(), "tcp", f.echo)
	require.ErrorIs(t, err, ErrTunnel)
	assert.Contains(t, err.Error(), "HOST KEY MISMATCH")

	assert.Zero(t, f.Server.Stats().AuthTries)
}

func TestMissingKnownHostsFileIsAnError(t *testing.T) {
	f := newFixture(t)
	f.Config.KnownHosts = t.TempDir() + "/nope"

	_, err := New(f.Config, 5*time.Second).DialContext(context.Background(), "tcp", f.echo)
	require.ErrorIs(t, err, ErrTunnel)
	assert.Contains(t, err.Error(), "known_hosts")
}

func TestKnownHostsWithOnlyTheEd25519KeyStillVerifiesAnRSAPreferringServer(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	client, clientPriv := sshtest.Ed25519(t)
	edHost, _ := sshtest.Ed25519(t)
	echo := sshtest.Echo(t)

	// The server prefers RSA. known_hosts has only its ed25519 key, which is
	// how many files look after `ssh-keygen -F` or a hand-copied line.
	srv := sshtest.Start(t, client.PublicKey(), []ssh.Signer{sshtest.RSA(t), edHost}, echo)

	tun := New(config.SSH{
		Host: srv.Host(), Port: srv.Port(), User: "reader",
		Key:        sshtest.WriteKey(t, clientPriv, ""),
		KnownHosts: sshtest.WriteKnownHosts(t, srv.Addr, edHost.PublicKey()),
	}, 5*time.Second)
	defer tun.Close()

	c, err := tun.DialContext(context.Background(), "tcp", echo)
	require.NoError(t, err)
	roundTrip(t, c, "ok")
}

func TestWrongKeyFailsAuthentication(t *testing.T) {
	f := newFixture(t)

	_, stranger := sshtest.Ed25519(t)
	f.Config.Key = sshtest.WriteKey(t, stranger, "")

	_, err := New(f.Config, 5*time.Second).DialContext(context.Background(), "tcp", f.echo)
	require.ErrorIs(t, err, ErrTunnel)
	assert.Contains(t, err.Error(), "unable to authenticate")
}

func TestPassphraseProtectedKey(t *testing.T) {
	f := newFixture(t)

	f.Config.Key = sshtest.WriteKey(t, f.ClientPriv, "s3cret")

	_, err := New(f.Config, 5*time.Second).DialContext(context.Background(), "tcp", f.echo)
	require.ErrorIs(t, err, ErrTunnel)
	assert.Contains(t, err.Error(), "passphrase-protected")

	t.Setenv(PassphraseEnv, "wrong")
	_, err = New(f.Config, 5*time.Second).DialContext(context.Background(), "tcp", f.echo)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decrypt key")

	t.Setenv(PassphraseEnv, "s3cret")
	tun := New(f.Config, 5*time.Second)
	defer tun.Close()

	c, err := tun.DialContext(context.Background(), "tcp", f.echo)
	require.NoError(t, err)
	roundTrip(t, c, "ok")
}

func TestNoCredentialsIsAnError(t *testing.T) {
	f := newFixture(t)
	f.Config.Key = ""

	_, err := New(f.Config, 5*time.Second).DialContext(context.Background(), "tcp", f.echo)
	require.ErrorIs(t, err, ErrTunnel)
	assert.Contains(t, err.Error(), "no credentials")
}

func TestSilentServerCannotHoldTheHandshake(t *testing.T) {
	f := newFixture(t)

	// Accepts TCP, never sends a banner.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			t.Cleanup(func() { _ = c.Close() })
		}
	}()

	_, port, _ := net.SplitHostPort(ln.Addr().String())
	f.Config.Port, _ = strconv.Atoi(port)
	f.Config.KnownHosts = sshtest.WriteKnownHosts(t, ln.Addr().String(), f.HostKey.PublicKey())

	for name, run := range map[string]func() error{
		"fallback timeout": func() error {
			_, err := New(f.Config, 300*time.Millisecond).DialContext(context.Background(), "tcp", f.echo)

			return err
		},
		"caller deadline": func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()

			_, err := New(f.Config, time.Hour).DialContext(ctx, "tcp", f.echo)

			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			err := run()

			require.ErrorIs(t, err, ErrTunnel)
			assert.Contains(t, err.Error(), "handshake")
			assert.Less(t, time.Since(start), 3*time.Second)
		})
	}
}

func TestOnlyTCPIsForwarded(t *testing.T) {
	_, err := New(config.SSH{Host: "127.0.0.1", User: "u"}, time.Second).DialContext(context.Background(), "unix", "/var/run/mysqld/mysqld.sock")
	require.ErrorIs(t, err, ErrTunnel)
	assert.Contains(t, err.Error(), "only tcp")
}

func TestStringNamesTheHop(t *testing.T) {
	tun := New(config.SSH{Host: "127.0.0.1", User: "reader"}, time.Second)
	assert.Equal(t, "reader@127.0.0.1:22", tun.String(), "port defaults to 22")
}
