// Package tunnel forwards database connections through an SSH host, in
// process, so svidoq can reach a server that listens only on the SSH host's
// loopback without anyone opening a tunnel for it beforehand.
//
// The SSH connection is opened on the first dial and closed by Close, which
// the datasource calls when the command ends: a forward lives exactly as long
// as the command that needed it. What the tunnel does not do is as deliberate
// as what it does:
//
//   - The host key must already be in known_hosts. An unknown host is refused
//     the same as a changed one; nothing is ever added to the file.
//   - Only direct-tcpip channels are opened. No session, no shell, no command,
//     and so no agent forwarding: the agent, when present, signs the login and
//     is never offered to the server.
//   - Connect, handshake and channel open are bounded by the caller's context,
//     and by a fallback timeout when the context has none.
package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/iamnikolie/svidoq/internal/config"
)

// PassphraseEnv names the variable that decrypts a passphrase-protected key
// when ssh-agent does not hold it.
const PassphraseEnv = "SVIDOQ_SSH_PASSPHRASE"

// ErrTunnel marks a failure on the SSH side, before the database was reached.
var ErrTunnel = errors.New("ssh tunnel")

// Tunnel dials addresses as seen from the SSH host.
type Tunnel struct {
	cfg     config.SSH
	timeout time.Duration

	mu     sync.Mutex
	client *ssh.Client
	closed bool
}

// New prepares a tunnel; nothing is dialed until the first DialContext. The
// timeout bounds connect and handshake when the caller's context has no
// deadline of its own.
func New(cfg config.SSH, timeout time.Duration) *Tunnel {
	if cfg.Port == 0 {
		cfg.Port = config.DefaultSSHPort
	}

	if cfg.KnownHosts == "" {
		cfg.KnownHosts = config.DefaultKnownHosts
	}

	return &Tunnel{cfg: cfg, timeout: timeout}
}

// String names the hop: user@host:port.
func (t *Tunnel) String() string { return t.cfg.String() }

// DialContext opens a connection to addr from the SSH host. It has the shape
// of the MySQL driver's DialFunc. Only TCP can be forwarded.
func (t *Tunnel) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("%w: only tcp addresses can be forwarded, not %s %s", ErrTunnel, network, addr)
	}

	client, err := t.connect(ctx)
	if err != nil {
		return nil, err
	}

	conn, err := client.DialContext(ctx, "tcp", addr)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("%w: open %s via %s: %w", ErrTunnel, addr, t, ctxErr)
		}

		return nil, fmt.Errorf("%w: open %s via %s (the ssh server refused the forward): %w", ErrTunnel, addr, t, err)
	}

	return conn, nil
}

// Close tears down the SSH connection and every forward on it. A closed
// tunnel cannot be dialed again.
func (t *Tunnel) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.closed = true

	if t.client == nil {
		return nil
	}

	err := t.client.Close()
	t.client = nil

	if errors.Is(err, net.ErrClosed) {
		return nil
	}

	return err
}

// connect returns the SSH client, opening it on first use. A failed attempt
// is not remembered, so a pool retry gets a fresh one.
func (t *Tunnel) connect(ctx context.Context) (*ssh.Client, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil, fmt.Errorf("%w: %s is closed", ErrTunnel, t)
	}

	if t.client != nil {
		return t.client, nil
	}

	client, err := t.dial(ctx)
	if err != nil {
		return nil, err
	}

	t.client = client

	return client, nil
}

func (t *Tunnel) dial(ctx context.Context) (*ssh.Client, error) {
	if _, ok := ctx.Deadline(); !ok && t.timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, t.timeout)
		defer cancel()
	}

	hostKey, known, err := hostKeyCallback(t.cfg.KnownHosts, t.cfg.Host)
	if err != nil {
		return nil, err
	}

	auth, closeAgent, err := authMethods(t.cfg)
	if err != nil {
		return nil, err
	}
	defer closeAgent()

	addr := net.JoinHostPort(t.cfg.Host, strconv.Itoa(t.cfg.Port))

	var d net.Dialer

	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%w: connect %s: %w", ErrTunnel, addr, err)
	}

	// The handshake reads from a plain socket and knows nothing of contexts:
	// a server that accepts and then says nothing would hold it forever. The
	// deadline bounds it, and a cancel closes the socket under it.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User:              t.cfg.User,
		Auth:              auth,
		HostKeyCallback:   hostKey,
		HostKeyAlgorithms: hostKeyAlgorithms(known, addr, conn.RemoteAddr()),
	})

	if stopped := stop(); err != nil || !stopped {
		_ = conn.Close()

		// The handshake finished, but the cancel had already closed the
		// socket under it: the connection is dead either way.
		if err == nil {
			_ = sshConn.Close()

			err = context.Cause(ctx)
		} else if ctxErr := ctx.Err(); ctxErr != nil {
			err = fmt.Errorf("%w (%w)", ctxErr, err)
		}

		return nil, fmt.Errorf("%w: handshake with %s@%s: %w", ErrTunnel, t.cfg.User, addr, err)
	}

	_ = conn.SetDeadline(time.Time{})

	return ssh.NewClient(sshConn, chans, reqs), nil
}

// hostKeyCallback checks the server's key against known_hosts, strictly: an
// unknown host fails the same as a changed key, and the file is never written.
// The second callback is the bare known_hosts lookup, for hostKeyAlgorithms.
func hostKeyCallback(path, host string) (ssh.HostKeyCallback, ssh.HostKeyCallback, error) {
	path = expandHome(path)

	cb, err := knownhosts.New(path)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: read known_hosts %s (add %s's host key there first; svidoq never adds one): %w",
			ErrTunnel, path, host, err)
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := cb(hostname, remote, key)

		var ke *knownhosts.KeyError
		if errors.As(err, &ke) {
			if len(ke.Want) == 0 {
				return fmt.Errorf("host key for %s is not in %s; svidoq does not accept new keys — "+
					"verify the fingerprint and add it there (%s %s)",
					hostname, path, key.Type(), ssh.FingerprintSHA256(key))
			}

			return fmt.Errorf("HOST KEY MISMATCH for %s: %s holds a different key than the server offered (%s %s); "+
				"refusing to connect — this is what a man-in-the-middle looks like, or a reinstalled host",
				hostname, path, key.Type(), ssh.FingerprintSHA256(key))
		}

		return err
	}, cb, nil
}

// hostKeyAlgorithms asks the server only for key types known_hosts holds for
// it. Without this the handshake negotiates whatever the server prefers — say
// RSA — and then fails against a file that only has the host's ed25519 key,
// although that key would have verified fine. Nil means the library default.
// cb must be the bare knownhosts callback: the probe reads its KeyError.
func hostKeyAlgorithms(cb ssh.HostKeyCallback, hostport string, remote net.Addr) []string {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil
	}

	probe, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil
	}

	var ke *knownhosts.KeyError
	if err := cb(hostport, remote, probe); !errors.As(err, &ke) || len(ke.Want) == 0 {
		return nil
	}

	var algos []string

	seen := map[string]bool{}

	for _, k := range ke.Want {
		types := []string{k.Key.Type()}
		if k.Key.Type() == ssh.KeyAlgoRSA {
			types = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
		}

		for _, a := range types {
			if !seen[a] {
				seen[a] = true

				algos = append(algos, a)
			}
		}
	}

	return algos
}

// authMethods mirrors plain ssh: the configured key first (decrypted with
// $SVIDOQ_SSH_PASSPHRASE when it is protected), then the keys ssh-agent holds.
// A protected key with no passphrase is skipped when the agent can sign for it.
func authMethods(c config.SSH) ([]ssh.AuthMethod, func(), error) {
	var (
		methods      []ssh.AuthMethod
		agentSigners func() ([]ssh.Signer, error)
	)

	closeAgent := func() {}

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			agentSigners = agent.NewClient(conn).Signers
			closeAgent = func() { _ = conn.Close() }
		}
	}

	fail := func(err error) ([]ssh.AuthMethod, func(), error) {
		closeAgent()

		return nil, func() {}, fmt.Errorf("%w: %w", ErrTunnel, err)
	}

	if c.Key != "" {
		path := expandHome(c.Key)

		raw, err := os.ReadFile(path)
		if err != nil {
			return fail(fmt.Errorf("read key: %w", err))
		}

		signer, err := ssh.ParsePrivateKey(raw)

		var missing *ssh.PassphraseMissingError

		switch {
		case err == nil:
			methods = append(methods, ssh.PublicKeys(signer))
		case errors.As(err, &missing):
			pass := os.Getenv(PassphraseEnv)

			switch {
			case pass != "":
				signer, err = ssh.ParsePrivateKeyWithPassphrase(raw, []byte(pass))
				if err != nil {
					return fail(fmt.Errorf("decrypt key %s with $%s: %w", path, PassphraseEnv, err))
				}

				methods = append(methods, ssh.PublicKeys(signer))
			case agentSigners == nil:
				return fail(fmt.Errorf("key %s is passphrase-protected: add it to ssh-agent, or set $%s", path, PassphraseEnv))
			}
		default:
			return fail(fmt.Errorf("parse key %s: %w", path, err))
		}
	}

	if agentSigners != nil {
		methods = append(methods, ssh.PublicKeysCallback(agentSigners))
	}

	if len(methods) == 0 {
		return fail(errors.New("no credentials: set ssh.key, or run ssh-agent with the key added"))
	}

	return methods, closeAgent, nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}

	return p
}
