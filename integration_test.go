//go:build integration

package main

// Live integration test, opt-in: go test -tags integration -run Live ./...
//
// Builds the real binary, runs it in serve mode as a separate process, and
// drives it over HTTP against the local OpenRSC server, which must already
// be running. It reuses one fixed test account rather than registering a
// new one each run, because the game server rate-limits registration.
//
// Environment (all optional):
//   PLUTONIUM_IT_GAME  game server address  (default 127.0.0.1:43594)
//   PLUTONIUM_IT_USER  test account         (default "Fellow Tst1")
//   PLUTONIUM_IT_PASS  its password         (default "fellowpass1")

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// lockedBuffer collects the process's output safely across goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type live struct {
	t    *testing.T
	base string
}

func (l *live) call(method, path, body string) (int, map[string]any) {
	l.t.Helper()
	req, _ := http.NewRequest(method, l.base+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		l.t.Fatalf("%s %s: %s", method, path, err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// waitState polls the account until it reaches state, or fails.
func (l *live) waitState(key string, state accountState, within time.Duration) map[string]any {
	l.t.Helper()
	deadline := time.Now().Add(within)
	var last map[string]any
	for time.Now().Before(deadline) {
		_, last = l.call("GET", "/v1/accounts/"+key, "")
		if last["state"] == string(state) {
			return last
		}
		time.Sleep(250 * time.Millisecond)
	}
	l.t.Fatalf("account %s did not reach %s within %s; last %v", key, state, within, last)
	return nil
}

func TestLiveServeMode(t *testing.T) {
	game := envOr("PLUTONIUM_IT_GAME", "127.0.0.1:43594")
	user := envOr("PLUTONIUM_IT_USER", "Fellow Tst1")
	pass := envOr("PLUTONIUM_IT_PASS", "fellowpass1")
	key := accountKey(user)

	if conn, err := net.DialTimeout("tcp", game, 2*time.Second); err != nil {
		t.Fatalf("game server not listening on %s (start it first): %s", game, err)
	} else {
		conn.Close()
	}

	// Plutonium finds settings.toml and data/ next to its binary, so build
	// into the repo (the path is gitignored).
	if out, err := exec.Command("go", "build", "-o", "plutonium", ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %s\n%s", err, out)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	var output lockedBuffer
	cmd := exec.Command("./plutonium", "-serve", addr)
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		if t.Failed() {
			t.Logf("plutonium output:\n%s", output.String())
		}
	})

	l := &live{t: t, base: "http://" + addr}

	// Wait for the server to finish loading the world and start listening.
	ready := false
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if resp, err := http.Get(l.base + "/v1/accounts"); err == nil {
			resp.Body.Close()
			ready = true
			break
		}
	}
	if !ready {
		t.Fatal("serve mode never started listening")
	}

	// The fixed test account: created on the first run, taken thereafter.
	status, body := l.call("POST", "/v1/accounts", fmt.Sprintf(`{"username":%q,"password":%q,"email":"fellow@example.com"}`, user, pass))
	if status != 201 && status != 409 {
		t.Fatalf("registering test account: %d %v", status, body)
	}

	// Log in using a different spelling of the same name.
	if status, body := l.call("POST", "/v1/accounts/"+strings.ToUpper(strings.ReplaceAll(user, " ", "%20"))+"/login", fmt.Sprintf(`{"password":%q}`, pass)); status != 202 {
		t.Fatalf("login: %d %v", status, body)
	}
	first := l.waitState(key, stateOnline, 60*time.Second)

	for _, fn := range []string{"get_x", "get_z"} {
		status, body := l.call("GET", "/v1/accounts/"+key+"/fn/"+fn, "")
		if status != 200 {
			t.Fatalf("%s: %d %v", fn, status, body)
		}
		if v, ok := body["result"].(float64); !ok || v <= 0 {
			t.Fatalf("%s returned %v, want a positive coordinate", fn, body["result"])
		}
	}
	if status, body := l.call("POST", "/v1/accounts/"+key+"/fn/send_chat_message", `{"message":"integration test"}`); status != 200 {
		t.Fatalf("send_chat_message: %d %v", status, body)
	}

	// Logout ends in stopped, even when the server refuses the logout packet
	// (it does right after login) and the grace close has to step in.
	if status, _ := l.call("POST", "/v1/accounts/"+key+"/logout", ""); status != 202 {
		t.Fatalf("logout: %d", status)
	}
	l.waitState(key, stateStopped, 30*time.Second)

	// A wrong password is fatal: login_failed, no retrying.
	if status, body := l.call("POST", "/v1/accounts/"+key+"/login", `{"password":"definitelywrong"}`); status != 202 {
		t.Fatalf("wrong-password login: %d %v", status, body)
	}
	failed := l.waitState(key, stateLoginFailed, 60*time.Second)
	if failed["reason"] != "invalid_credentials" {
		t.Fatalf("login_failed reason %v, want invalid_credentials", failed["reason"])
	}

	// Logging back in starts a new session.
	if status, body := l.call("POST", "/v1/accounts/"+key+"/login", fmt.Sprintf(`{"password":%q}`, pass)); status != 202 {
		t.Fatalf("relogin: %d %v", status, body)
	}
	second := l.waitState(key, stateOnline, 60*time.Second)
	if second["session"].(float64) != first["session"].(float64)+1 {
		t.Fatalf("session went %v -> %v, want +1", first["session"], second["session"])
	}

	// SIGTERM logs everyone out and exits cleanly.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("exit after SIGTERM: %s", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("did not exit within 30s of SIGTERM")
	}
	if !strings.Contains(output.String(), "[BOT] Shut down") {
		t.Fatal("no clean shutdown message in output")
	}
}
