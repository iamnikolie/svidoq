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
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
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

	// life is cancelled by Close, which aborts a connect in progress — its
	// sockets, the agent's included, are closed under it.
	life     context.Context
	shutdown context.CancelFunc

	// connecting admits one connect attempt at a time. It is a channel, not a
	// mutex, so a waiter still honours its own context and Close.
	connecting chan struct{}

	// mu guards the fields below and is never held across network IO.
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

	life, shutdown := context.WithCancel(context.Background())

	return &Tunnel{cfg: cfg, timeout: timeout, life: life, shutdown: shutdown, connecting: make(chan struct{}, 1)}
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
	t.shutdown()

	t.mu.Lock()
	t.closed = true
	client := t.client
	t.client = nil
	t.mu.Unlock()

	if client == nil {
		return nil
	}

	err := client.Close()

	if errors.Is(err, net.ErrClosed) {
		return nil
	}

	return err
}

// connect returns the SSH client, opening it on first use. A failed attempt
// is not remembered, so a pool retry gets a fresh one.
func (t *Tunnel) connect(ctx context.Context) (*ssh.Client, error) {
	if client, err := t.current(); client != nil || err != nil {
		return client, err
	}

	select {
	case t.connecting <- struct{}{}:
		defer func() { <-t.connecting }()
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: waiting to connect to %s: %w", ErrTunnel, t, ctx.Err())
	case <-t.life.Done():
		return nil, t.closedError()
	}

	// Another caller may have connected while this one waited.
	if client, err := t.current(); client != nil || err != nil {
		return client, err
	}

	// The attempt ends at the caller's deadline or at Close, whichever is first.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stop := context.AfterFunc(t.life, cancel)
	defer stop()

	client, err := t.dial(ctx)
	if err != nil {
		if t.life.Err() != nil {
			return nil, t.closedError()
		}

		return nil, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		_ = client.Close()

		return nil, t.closedError()
	}

	t.client = client

	return client, nil
}

// current returns the open client, an error when the tunnel is closed, or
// neither when a connect is needed.
func (t *Tunnel) current() (*ssh.Client, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil, t.closedError()
	}

	return t.client, nil
}

func (t *Tunnel) closedError() error {
	return fmt.Errorf("%w: %s is closed", ErrTunnel, t)
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

	auth, closeAgent, err := authMethods(ctx, t.cfg)
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
//
// An @cert-authority line says nothing about the host key's own type — its
// key is the CA's — so it contributes the certificate algorithms instead, and
// the strict callback then checks the certificate against that CA. Plain host
// keys come first, so a host known both ways is verified by its own key.
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

	var (
		algos []string
		ca    bool
	)

	seen := map[string]bool{}
	add := func(types ...string) {
		for _, a := range types {
			if !seen[a] {
				seen[a] = true

				algos = append(algos, a)
			}
		}
	}

	lines := knownHostsLines{}

	for _, k := range ke.Want {
		switch {
		case lines.isCertAuthority(k.Filename, k.Line):
			ca = true
		case k.Key.Type() == ssh.KeyAlgoRSA:
			add(ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
		default:
			add(k.Key.Type())
		}
	}

	if ca {
		add(certAlgorithms...)
	}

	return algos
}

// certAlgorithms are the host certificate types offered when known_hosts
// trusts a CA for the host. SHA-1 RSA and DSA are left out, as for plain keys.
var certAlgorithms = []string{
	ssh.CertAlgoED25519v01,
	ssh.CertAlgoECDSA256v01, ssh.CertAlgoECDSA384v01, ssh.CertAlgoECDSA521v01,
	ssh.CertAlgoRSASHA512v01, ssh.CertAlgoRSASHA256v01,
	ssh.CertAlgoSKED25519v01, ssh.CertAlgoSKECDSA256v01,
}

// knownHostsLines tells which known_hosts lines are @cert-authority entries.
// knownhosts reports matching CA lines among the host's keys, with only their
// file and line number to tell them apart.
type knownHostsLines map[string][]string

func (l knownHostsLines) isCertAuthority(file string, line int) bool {
	text, ok := l[file]
	if !ok {
		raw, err := os.ReadFile(file)
		if err == nil {
			text = strings.Split(string(raw), "\n")
		}

		l[file] = text
	}

	if line < 1 || line > len(text) {
		return false
	}

	fields := strings.Fields(text[line-1])

	return len(fields) > 0 && fields[0] == "@cert-authority"
}

// authMethods mirrors plain ssh: the configured key first (decrypted with
// $SVIDOQ_SSH_PASSPHRASE when it is protected), then the keys ssh-agent holds.
// A protected key with no passphrase is skipped when the agent can sign for it.
//
// The agent is a second socket the handshake waits on: an agent that never
// answers — or waits for a confirmation nobody gives — must not outlast ctx.
// Its connection carries ctx's deadline and is closed when ctx ends, which
// fails a pending list or sign call instead of leaving it blocked.
func authMethods(ctx context.Context, c config.SSH) ([]ssh.AuthMethod, func(), error) {
	var (
		methods      []ssh.AuthMethod
		agentSigners func() ([]ssh.Signer, error)
	)

	closeAgent := func() {}

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		var d net.Dialer

		if conn, err := d.DialContext(ctx, "unix", sock); err == nil {
			if deadline, ok := ctx.Deadline(); ok {
				_ = conn.SetDeadline(deadline)
			}

			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			agentSigners = agent.NewClient(conn).Signers
			closeAgent = func() {
				stop()
				_ = conn.Close()
			}
		}
	}

	fail := func(err error) ([]ssh.AuthMethod, func(), error) {
		closeAgent()

		return nil, func() {}, fmt.Errorf("%w: %w", ErrTunnel, err)
	}

	if c.Key != "" {
		path := expandHome(c.Key)

		raw, err := readPrivateKey(path)
		if err != nil {
			return fail(err)
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

// readPrivateKey reads a key file the way ssh would agree to use it: a regular
// file that no one but its owner can read or write. A key others can read is
// a key others may already have; ssh ignores such a file, and so does svidoq —
// it says so rather than fixing the mode itself. Windows has no such mode
// bits; its ACLs are left to the operating system.
func readPrivateKey(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read key: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("read key: %w", err)
	}

	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("key %s is not a regular file", path)
	}

	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("key %s is accessible by others (mode %04o); refusing to use it, as ssh does — run chmod 600 %s",
			path, info.Mode().Perm(), path)
	}

	raw, err := io.ReadAll(io.LimitReader(f, maxKeyBytes))
	if err != nil {
		return nil, fmt.Errorf("read key: %w", err)
	}

	return raw, nil
}

// maxKeyBytes is far above any real private key; a path pointing at something
// else is not read whole.
const maxKeyBytes = 1 << 20

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}

	return p
}
