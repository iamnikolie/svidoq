package tunnel

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/iamnikolie/svidoq/internal/config"
	"github.com/iamnikolie/svidoq/internal/tunnel/sshtest"
)

// agentSocket serves ag on a fresh unix socket and points SSH_AUTH_SOCK at
// it. The directory is short: unix socket paths have a small length limit.
func agentSocket(t *testing.T, serve func(net.Conn)) {
	t.Helper()

	dir, err := os.MkdirTemp("", "svq-agent-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	ln, err := net.Listen("unix", filepath.Join(dir, "sock"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	t.Setenv("SSH_AUTH_SOCK", ln.Addr().String())

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			t.Cleanup(func() { _ = c.Close() })

			go serve(c)
		}
	}()
}

// silentAgent accepts and never answers, like an agent stuck or waiting on a
// confirmation nobody gives.
func silentAgent(c net.Conn) { _, _ = io.Copy(io.Discard, c) }

// stallingSigner lists its keys, then never finishes a signature.
type stallingSigner struct {
	agent.Agent

	release chan struct{}
}

func (s stallingSigner) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	<-s.release

	return nil, errors.New("released")
}

// agentOnly is a fixture that logs in with ssh-agent alone.
func agentOnly(t *testing.T, serve func(net.Conn)) fixture {
	t.Helper()

	f := newFixture(t)
	f.Config.Key = ""
	agentSocket(t, serve)

	return f
}

func TestAgentLogin(t *testing.T) {
	keyring := agent.NewKeyring()

	f := agentOnly(t, func(c net.Conn) { _ = agent.ServeAgent(keyring, c) })
	require.NoError(t, keyring.Add(agent.AddedKey{PrivateKey: f.ClientPriv}))

	tun := New(f.Config, 5*time.Second)
	defer tun.Close()

	c, err := tun.DialContext(context.Background(), "tcp", f.echo)
	require.NoError(t, err)
	roundTrip(t, c, "agent")

	assert.Empty(t, f.Server.Stats().Requests, "the agent signs; it is never forwarded")
}

// A stuck agent is a second socket the handshake waits on. Neither the
// caller's deadline nor Close may wait for it.
func TestStuckAgentObeysTheDeadline(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	keyring := agent.NewKeyring()
	stalling := stallingSigner{Agent: keyring, release: release}

	for name, serve := range map[string]func(net.Conn){
		"silent on list": silentAgent,
		"silent on sign": func(c net.Conn) { _ = agent.ServeAgent(stalling, c) },
	} {
		t.Run(name, func(t *testing.T) {
			f := agentOnly(t, serve)
			require.NoError(t, keyring.Add(agent.AddedKey{PrivateKey: f.ClientPriv}))

			tun := New(f.Config, time.Hour)

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()

			start := time.Now()
			_, err := tun.DialContext(ctx, "tcp", f.echo)

			require.ErrorIs(t, err, ErrTunnel)
			assert.Less(t, time.Since(start), 2*time.Second, "returned at the deadline, not when the agent answered")

			closed := make(chan error, 1)
			go func() { closed <- tun.Close() }()

			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("Close blocked behind the failed connect")
			}
		})
	}
}

func TestStuckAgentFallsBackToTheTimeout(t *testing.T) {
	f := agentOnly(t, silentAgent)

	start := time.Now()
	_, err := New(f.Config, 200*time.Millisecond).DialContext(context.Background(), "tcp", f.echo)

	require.ErrorIs(t, err, ErrTunnel)
	assert.Less(t, time.Since(start), 2*time.Second)
}

// Close must not wait for a connect in progress: it aborts it.
func TestCloseAbortsAConnectInProgress(t *testing.T) {
	f := agentOnly(t, silentAgent)

	tun := New(f.Config, time.Hour)

	done := make(chan error, 1)
	go func() {
		_, err := tun.DialContext(context.Background(), "tcp", f.echo)
		done <- err
	}()

	// Let the connect reach the agent.
	time.Sleep(200 * time.Millisecond)

	closed := make(chan error, 1)
	go func() { closed <- tun.Close() }()

	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Close blocked behind the connect")
	}

	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrTunnel)
		assert.Contains(t, err.Error(), "closed")
	case <-time.After(2 * time.Second):
		t.Fatal("the connect outlived Close")
	}
}

// A waiter behind a stuck connect still honours its own deadline.
func TestWaitingForAConnectObeysTheDeadline(t *testing.T) {
	f := agentOnly(t, silentAgent)

	tun := New(f.Config, time.Hour)
	defer tun.Close()

	go func() { _, _ = tun.DialContext(context.Background(), "tcp", f.echo) }()

	time.Sleep(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := tun.DialContext(ctx, "tcp", f.echo)

	require.ErrorIs(t, err, ErrTunnel)
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestPrivateKeyModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix mode bits")
	}

	for mode, ok := range map[os.FileMode]bool{
		0o600: true,
		0o400: true,
		0o640: false,
		0o604: false,
		0o644: false,
		0o660: false,
	} {
		t.Run(mode.String(), func(t *testing.T) {
			f := newFixture(t)
			require.NoError(t, os.Chmod(f.Config.Key, mode))

			tun := New(f.Config, 5*time.Second)
			defer tun.Close()

			c, err := tun.DialContext(context.Background(), "tcp", f.echo)
			if ok {
				require.NoError(t, err)
				_ = c.Close()

				return
			}

			require.ErrorIs(t, err, ErrTunnel)
			assert.Contains(t, err.Error(), "accessible by others")
			assert.Contains(t, err.Error(), "chmod 600")

			info, statErr := os.Stat(f.Config.Key)
			require.NoError(t, statErr)
			assert.Equal(t, mode, info.Mode().Perm(), "svidoq reports the mode; it does not change it")
		})
	}
}

func TestKeyThatIsNotAFileIsRefused(t *testing.T) {
	f := newFixture(t)
	f.Config.Key = t.TempDir()

	_, err := New(f.Config, 5*time.Second).DialContext(context.Background(), "tcp", f.echo)
	require.ErrorIs(t, err, ErrTunnel)
	assert.Contains(t, err.Error(), "not a regular file")
}

func TestErrorsDoNotPrintPassphraseOrKey(t *testing.T) {
	f := newFixture(t)
	f.Config.Key = sshtest.WriteKey(t, f.ClientPriv, "actual-secret")
	t.Setenv(PassphraseEnv, "wrong-secret")

	raw, err := os.ReadFile(f.Config.Key)
	require.NoError(t, err)

	_, err = New(f.Config, time.Second).DialContext(context.Background(), "tcp", f.echo)
	require.Error(t, err)

	for _, secret := range []string{"actual-secret", "wrong-secret", string(raw)} {
		assert.NotContains(t, err.Error(), secret)
	}
}

// hostCertServer presents a host certificate signed by ca, for 127.0.0.1.
func hostCertServer(t *testing.T, client ssh.PublicKey, ca ssh.Signer, echo string) (*sshtest.Server, ssh.Signer) {
	t.Helper()

	host, _ := sshtest.Ed25519(t)
	cert := &ssh.Certificate{
		Key:             host.PublicKey(),
		CertType:        ssh.HostCert,
		ValidPrincipals: []string{"127.0.0.1"},
		ValidBefore:     ssh.CertTimeInfinity,
	}
	require.NoError(t, cert.SignCert(rand.Reader, ca))

	signer, err := ssh.NewCertSigner(cert, host)
	require.NoError(t, err)

	return sshtest.Start(t, client, []ssh.Signer{signer}, echo), host
}

func caLine(addr string, ca ssh.PublicKey) string {
	return "@cert-authority " + knownhosts.Line([]string{knownhosts.Normalize(addr)}, ca) + "\n"
}

func writeFile(t *testing.T, body string) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), "known_hosts")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))

	return p
}

func TestHostCertificateSignedByATrustedCA(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")

	client, priv := sshtest.Ed25519(t)
	ca, _ := sshtest.Ed25519(t)
	other, _ := sshtest.Ed25519(t)
	echo := sshtest.Echo(t)
	srv, hostKey := hostCertServer(t, client.PublicKey(), ca, echo)

	dial := func(knownHosts string) error {
		tun := New(config.SSH{
			Host: srv.Host(), Port: srv.Port(), User: "reader",
			Key: sshtest.WriteKey(t, priv, ""), KnownHosts: knownHosts,
		}, 5*time.Second)
		defer tun.Close()

		c, err := tun.DialContext(context.Background(), "tcp", echo)
		if err == nil {
			_ = c.Close()
		}

		return err
	}

	t.Run("CA only", func(t *testing.T) {
		require.NoError(t, dial(writeFile(t, "# comment\n\n"+caLine(srv.Addr, ca.PublicKey()))))
	})

	t.Run("CA and the plain host key", func(t *testing.T) {
		require.NoError(t, dial(writeFile(t, caLine(srv.Addr, ca.PublicKey())+
			knownhosts.Line([]string{knownhosts.Normalize(srv.Addr)}, hostKey.PublicKey())+"\n")))
	})

	t.Run("untrusted CA is refused", func(t *testing.T) {
		err := dial(writeFile(t, caLine(srv.Addr, other.PublicKey())))
		require.ErrorIs(t, err, ErrTunnel)
	})

	t.Run("CA for another host is refused", func(t *testing.T) {
		err := dial(writeFile(t, caLine("203.0.113.7:22", ca.PublicKey())))
		require.ErrorIs(t, err, ErrTunnel)
		assert.Contains(t, err.Error(), "no authorities")
	})
}

func TestCertificateAlgorithmsComeOnlyFromCALines(t *testing.T) {
	addr := "127.0.0.1:2222"
	ca, _ := sshtest.Ed25519(t)
	edHost, _ := sshtest.Ed25519(t)
	rsaHost := sshtest.RSA(t)
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2222}

	algos := func(body string) []string {
		cb, err := knownhosts.New(writeFile(t, body))
		require.NoError(t, err)

		return hostKeyAlgorithms(cb, addr, remote)
	}

	plain := func(k ssh.PublicKey) string {
		return knownhosts.Line([]string{knownhosts.Normalize(addr)}, k) + "\n"
	}

	assert.Equal(t, []string{ssh.KeyAlgoED25519}, algos(plain(edHost.PublicKey())))
	assert.Equal(t, []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}, algos(plain(rsaHost.PublicKey())))
	assert.Equal(t, certAlgorithms, algos(caLine(addr, ca.PublicKey())),
		"a CA's own key type says nothing about the host key")
	assert.Equal(t, append([]string{ssh.KeyAlgoED25519}, certAlgorithms...),
		algos(caLine(addr, ca.PublicKey())+plain(edHost.PublicKey())), "plain keys first")
	assert.Nil(t, algos(knownhosts.Line([]string{"203.0.113.7"}, edHost.PublicKey())+"\n"), "unknown host: library default")
	assert.True(t, strings.HasSuffix(certAlgorithms[0], "-cert-v01@openssh.com"))
}
