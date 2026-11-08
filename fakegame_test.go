package main

// A fake game server (FELLOW-12, test framework, piece T1c). It speaks just
// enough of the RSC protocol for the real client to log in, so tests can
// drive the real session goroutine, event loop and packet handlers through
// a whole login, push chosen server packets, and see what the client sends.

import (
	"bytes"
	"encoding/binary"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"
)

type fakeGame struct {
	t  *testing.T
	ln net.Listener

	mu        sync.Mutex
	loginCode byte // reply to the next login; 64 is success
	logins    []string
	conn      net.Conn    // the logged-in connection, if any
	packets   chan []byte // frames the client sent after login: opcode, then payload
	accepted  chan struct{}
}

// startFakeGame listens on loopback and points the client at it.
func startFakeGame(t *testing.T) *fakeGame {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGame{
		t:         t,
		ln:        ln,
		loginCode: 64,
		packets:   make(chan []byte, 64),
		accepted:  make(chan struct{}, 8),
	}
	setVar(t, &settings.ServerSettings, &serverSettings{Address: ln.Addr().String()})
	setVar(t, &settings.ClientSettings, &clientSettings{ClientVersion: 1})
	// Any odd modulus works: the fake never decrypts the password block.
	// (A power of two would turn the even plaintext into zero bytes.)
	setVar(t, &rsaExp, big.NewInt(65537))
	setVar(t, &rsaMod, new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 1024), big.NewInt(1)))

	go g.serve()
	t.Cleanup(func() {
		ln.Close()
		g.mu.Lock()
		if g.conn != nil {
			g.conn.Close()
		}
		g.mu.Unlock()
	})
	return g
}

// grantLogins hands out login slots without the six-second stagger, until
// the test ends. (bot.loginWaiter's goroutine never exits, so it would
// outlive the test.)
func grantLogins(t *testing.T, b *bot) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-b.loginWait:
			case <-stop:
				return
			}
		}
	}()
}

func (g *fakeGame) serve() {
	for {
		conn, err := g.ln.Accept()
		if err != nil {
			return
		}
		go g.handle(conn)
	}
}

func readFrame(r io.Reader) ([]byte, error) {
	var n [2]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	frame := make([]byte, int(n[0])<<8|int(n[1]))
	_, err := io.ReadFull(r, frame)
	return frame, err
}

func (g *fakeGame) handle(conn net.Conn) {
	// Session id, then the login frame: opcode 0, a byte, the client
	// version, then the encoded username up to a newline.
	if _, err := conn.Write([]byte{0, 0, 0, 1}); err != nil {
		conn.Close()
		return
	}
	frame, err := readFrame(conn)
	if err != nil || len(frame) < 6 || frame[0] != 0 {
		conn.Close()
		return
	}
	name, _, _ := bytes.Cut(frame[6:], []byte{10})

	g.mu.Lock()
	code := g.loginCode
	g.logins = append(g.logins, string(name))
	g.mu.Unlock()

	if _, err := conn.Write([]byte{code}); err != nil || code&0x40 == 0 {
		conn.Close()
		return
	}

	g.mu.Lock()
	g.conn = conn
	g.mu.Unlock()
	g.accepted <- struct{}{}

	for {
		frame, err := readFrame(conn)
		if err != nil {
			return
		}
		switch frame[0] {
		case 67: // keepalive
			continue
		case 102: // logout: accept it, as the real server does when idle
			g.packets <- frame
			conn.Close()
			return
		}
		g.packets <- frame
	}
}

// loginNames returns the usernames of every login attempt, as sent on the wire.
func (g *fakeGame) loginNames() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string{}, g.logins...)
}

func (g *fakeGame) setLoginCode(code byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.loginCode = code
}

// waitLogin waits for a successful login connection.
func (g *fakeGame) waitLogin() {
	g.t.Helper()
	select {
	case <-g.accepted:
	case <-time.After(5 * time.Second):
		g.t.Fatal("client never logged in to the fake game server")
	}
}

// send writes one server packet. The client's reader treats the length
// prefix as including its own two bytes.
func (g *fakeGame) send(opcode byte, payload ...[]byte) {
	g.t.Helper()
	body := []byte{opcode}
	for _, p := range payload {
		body = append(body, p...)
	}
	n := len(body) + 2
	g.mu.Lock()
	conn := g.conn
	g.mu.Unlock()
	if conn == nil {
		g.t.Fatal("no logged-in connection to send on")
	}
	if _, err := conn.Write(append([]byte{byte(n >> 8), byte(n)}, body...)); err != nil {
		g.t.Fatalf("send opcode %d: %s", opcode, err)
	}
}

// nextPacket waits for the next frame the client sends and returns its opcode.
func (g *fakeGame) nextPacket() byte {
	g.t.Helper()
	select {
	case frame := <-g.packets:
		return frame[0]
	case <-time.After(5 * time.Second):
		g.t.Fatal("client sent no packet")
		return 0
	}
}

// --- packet builders ---------------------------------------------------------

func u8(v int) []byte  { return []byte{byte(v)} }
func u16(v int) []byte { return binary.BigEndian.AppendUint16(nil, uint16(v)) }
func u32(v int) []byte { return binary.BigEndian.AppendUint32(nil, uint32(v)) }
func str(s string) []byte {
	return append([]byte(s), 10)
}

// worldLoaded is the ignore list (opcode 109), the last packet of a login;
// the client treats it as "world loaded".
func (g *fakeGame) worldLoaded() { g.send(109, u8(0)) }

// serverMessage sends opcode 131. Type 0 is a game message, 4 is chat
// from a named sender, 6 a trade request.
func (g *fakeGame) serverMessage(msgType int, msg, sender string) {
	if sender == "" {
		g.send(131, u32(0), u8(msgType), u8(0), str(msg))
		return
	}
	g.send(131, u32(0), u8(msgType), u8(1), str(msg), str(sender), str(sender))
}
