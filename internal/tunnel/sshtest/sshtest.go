// Package sshtest runs an in-process SSH server shaped like a forward-only
// account, for tests: public-key login for one key, direct-tcpip to an
// allowlist (sshd's PermitOpen), and nothing else. It records what the client
// asked for, so a test can prove the client asked for nothing but forwards.
// It listens on loopback only and needs no network.
package sshtest

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/iamnikolie/svidoq/internal/config"
)

// Server is a running fake SSH server.
type Server struct {
	Addr string

	permit    map[string]bool
	clientKey ssh.PublicKey

	mu    sync.Mutex
	stats Stats
	conns []net.Conn
}

// Stats is what the client did on the server.
type Stats struct {
	Handshakes int
	AuthTries  int
	// Channels holds each channel type opened, with the target appended for
	// direct-tcpip ("direct-tcpip 127.0.0.1:3306").
	Channels []string
	// Requests holds global ("global:<type>") and channel ("channel:<type>")
	// requests.
	Requests []string
}

type directTCPIP struct {
	Host     string
	Port     uint32
	OrigHost string
	OrigPort uint32
}

var errDenied = errors.New("public key not accepted")

// Start serves until the test ends. hostKeys are offered in order of
// preference; permit lists the host:port forwards allowed.
func Start(t testing.TB, clientKey ssh.PublicKey, hostKeys []ssh.Signer, permit ...string) *Server {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("sshtest: listen: %v", err)
	}

	s := &Server{Addr: ln.Addr().String(), permit: map[string]bool{}, clientKey: clientKey}
	for _, p := range permit {
		s.permit[p] = true
	}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			s.mu.Lock()
			s.stats.AuthTries++
			s.mu.Unlock()

			if string(key.Marshal()) == string(s.clientKey.Marshal()) {
				return &ssh.Permissions{}, nil
			}

			return nil, errDenied
		},
	}
	for _, k := range hostKeys {
		cfg.AddHostKey(k)
	}

	t.Cleanup(func() {
		_ = ln.Close()

		s.mu.Lock()
		defer s.mu.Unlock()

		for _, c := range s.conns {
			_ = c.Close()
		}
	})

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			s.mu.Lock()
			s.conns = append(s.conns, c)
			s.mu.Unlock()

			go s.serve(c, cfg)
		}
	}()

	return s
}

// Host is the server's host, for an ssh block.
func (s *Server) Host() string {
	h, _, _ := net.SplitHostPort(s.Addr)

	return h
}

// Port is the server's port, for an ssh block.
func (s *Server) Port() int {
	_, p, _ := net.SplitHostPort(s.Addr)
	n, _ := strconv.Atoi(p)

	return n
}

// Stats returns a copy of what the client has done so far.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := s.stats
	out.Channels = append([]string(nil), s.stats.Channels...)
	out.Requests = append([]string(nil), s.stats.Requests...)

	return out
}

func (s *Server) serve(c net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sc.Close()

	s.mu.Lock()
	s.stats.Handshakes++
	s.mu.Unlock()

	go s.refuse(reqs, "global:")

	for nc := range chans {
		if nc.ChannelType() != "direct-tcpip" {
			s.record(&s.stats.Channels, nc.ChannelType())
			_ = nc.Reject(ssh.Prohibited, "forward-only account")

			continue
		}

		var p directTCPIP
		if err := ssh.Unmarshal(nc.ExtraData(), &p); err != nil {
			_ = nc.Reject(ssh.ConnectionFailed, "bad payload")

			continue
		}

		target := net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port)))
		s.record(&s.stats.Channels, "direct-tcpip "+target)

		if !s.permit[target] {
			_ = nc.Reject(ssh.Prohibited, "administratively prohibited: open failed")

			continue
		}

		go s.forward(nc, target)
	}
}

func (s *Server) refuse(reqs <-chan *ssh.Request, prefix string) {
	for r := range reqs {
		s.record(&s.stats.Requests, prefix+r.Type)

		if r.WantReply {
			_ = r.Reply(false, nil)
		}
	}
}

func (s *Server) forward(nc ssh.NewChannel, target string) {
	up, err := net.Dial("tcp", target)
	if err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, err.Error())

		return
	}

	ch, reqs, err := nc.Accept()
	if err != nil {
		_ = up.Close()

		return
	}

	go s.refuse(reqs, "channel:")

	go func() {
		_, _ = io.Copy(up, ch)
		_ = up.Close()
	}()

	_, _ = io.Copy(ch, up)
	_ = ch.Close()
}

func (s *Server) record(into *[]string, what string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	*into = append(*into, what)
}

// Echo starts a TCP server that echoes what it is sent — a stand-in database.
func Echo(t testing.TB) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("sshtest: listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			go func() {
				defer c.Close()

				_, _ = io.Copy(c, c)
			}()
		}
	}()

	return ln.Addr().String()
}

// Ed25519 makes a fresh key.
func Ed25519(t testing.TB) (ssh.Signer, ed25519.PrivateKey) {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("sshtest: key: %v", err)
	}

	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("sshtest: signer: %v", err)
	}

	return s, priv
}

// RSA makes a fresh 2048-bit key.
func RSA(t testing.TB) ssh.Signer {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("sshtest: key: %v", err)
	}

	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("sshtest: signer: %v", err)
	}

	return s
}

// WriteKey writes a private key in OpenSSH format, encrypted when a
// passphrase is given, and returns its path.
func WriteKey(t testing.TB, priv crypto.PrivateKey, passphrase string) string {
	t.Helper()

	var (
		block *pem.Block
		err   error
	)

	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(priv, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	}

	if err != nil {
		t.Fatalf("sshtest: marshal key: %v", err)
	}

	return writeFile(t, "id_test", pem.EncodeToMemory(block))
}

// WriteKnownHosts writes one known_hosts line per key for addr, as plain ssh
// would, and returns its path.
func WriteKnownHosts(t testing.TB, addr string, keys ...ssh.PublicKey) string {
	t.Helper()

	var b []byte
	for _, k := range keys {
		b = append(b, knownhosts.Line([]string{knownhosts.Normalize(addr)}, k)...)
		b = append(b, '\n')
	}

	return writeFile(t, "known_hosts", b)
}

func writeFile(t testing.TB, name string, data []byte) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("sshtest: write %s: %v", name, err)
	}

	return p
}

// Fixture is a working server, client key and known_hosts; tests break one
// piece at a time.
type Fixture struct {
	Server     *Server
	Config     config.SSH
	HostKey    ssh.Signer
	ClientPriv ed25519.PrivateKey
}

// New starts a server permitting forwards to permit and returns an ssh block
// that reaches it. ssh-agent is switched off for the test, so auth comes from
// the key file alone whatever the developer's shell has loaded.
func New(t testing.TB, permit ...string) Fixture {
	t.Helper()

	t.Setenv("SSH_AUTH_SOCK", "")

	client, clientPriv := Ed25519(t)
	host, _ := Ed25519(t)
	srv := Start(t, client.PublicKey(), []ssh.Signer{host}, permit...)

	return Fixture{
		Server:     srv,
		HostKey:    host,
		ClientPriv: clientPriv,
		Config: config.SSH{
			Host:       srv.Host(),
			Port:       srv.Port(),
			User:       "reader",
			Key:        WriteKey(t, clientPriv, ""),
			KnownHosts: WriteKnownHosts(t, srv.Addr, host.PublicKey()),
		},
	}
}
