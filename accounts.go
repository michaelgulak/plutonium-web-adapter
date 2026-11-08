package main

// Serve-mode account registry and session lifecycle.
//
// In serve mode the process starts with no accounts. The brain registers
// accounts on the game server and logs them in over HTTP; each logged-in
// account gets a session goroutine that owns its *client, mirroring
// runClientLoop but reporting state transitions and stopping on request.

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

type accountState string

const (
	stateLoggingIn    accountState = "logging_in"
	stateOnline       accountState = "online"
	stateDisconnected accountState = "disconnected"
	stateLoginFailed  accountState = "login_failed"
	stateStopping     accountState = "stopping"
	stateStopped      accountState = "stopped"
)

const (
	// How long a requested logout waits for the server to drop the
	// connection (it refuses while in combat) before closing the socket.
	logoutGrace = 10 * time.Second
)

const maxKeyLength = 12

// accountKey returns the canonical key for a username, or "" if the
// username cannot name an account.
//
// Two usernames get the same key exactly when they log in as the same
// account. Login sends encodeCredential(username), which maps each byte
// outside A-Za-z0-9 to '_'; the server then applies
// DataConversions.usernameToHash, which lowercases, treats every
// non-alphanumeric as a separator, trims separators at both ends and keeps
// at most 12 characters. So the key is built byte by byte with ASCII-only
// lowercasing (a multi-byte character becomes several separators, as on the
// wire), and runs of separators are kept because the server hashes each one.
//
// Instead of truncating past 12 characters, such usernames are rejected:
// the server only registers 2-12 character names, and a truncated key could
// end in '_', which would canonicalise differently when sent back.
func accountKey(username string) string {
	b := []byte(username)
	for i, ch := range b {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9':
		case ch >= 'A' && ch <= 'Z':
			b[i] = ch + ('a' - 'A')
		default:
			b[i] = '_'
		}
	}
	key := strings.Trim(string(b), "_")
	if len(key) > maxKeyLength {
		return ""
	}
	return key
}

// managedAccount is one account known to this serve-mode process.
// All fields are guarded by mu.
type managedAccount struct {
	mu        sync.Mutex
	key       string
	username  string
	password  string
	autologin bool
	state     accountState
	reason    string
	session   int
	since     time.Time
	client    *client       // non-nil only while online (world loaded); calls go here
	live      *client       // non-nil while connected, including while loading
	stop      chan struct{} // closed to request logout of the current run
	done      chan struct{} // closed when the current run's goroutine exits
	acct      *account
}

// accountView is the JSON shape of an account.
type accountView struct {
	Key      string       `json:"key"`
	Username string       `json:"username"`
	State    accountState `json:"state"`
	Reason   string       `json:"reason,omitempty"`
	Session  int          `json:"session"`
	Since    time.Time    `json:"since"`
}

func (ma *managedAccount) view() accountView {
	ma.mu.Lock()
	defer ma.mu.Unlock()
	return accountView{
		Key:      ma.key,
		Username: ma.username,
		State:    ma.state,
		Reason:   ma.reason,
		Session:  ma.session,
		Since:    ma.since,
	}
}

// setState records a transition and logs it. Callers must hold ma.mu.
func (ma *managedAccount) setStateLocked(state accountState, reason string) {
	ma.state = state
	ma.reason = reason
	ma.since = time.Now()
	if reason != "" {
		fmt.Printf("[%s] state -> %s (%s)\n", ma.key, state, reason)
	} else {
		fmt.Printf("[%s] state -> %s\n", ma.key, state)
	}
}

func (ma *managedAccount) setState(state accountState, reason string) {
	ma.mu.Lock()
	defer ma.mu.Unlock()
	ma.setStateLocked(state, reason)
}

// stopRequested reports whether logout has been requested for this run.
func (ma *managedAccount) stopRequested() bool {
	select {
	case <-ma.stop:
		return true
	default:
		return false
	}
}

// waitOrStop sleeps for d, returning false early if logout is requested.
func (ma *managedAccount) waitOrStop(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ma.stop:
		return false
	}
}

type registry struct {
	mu       sync.Mutex
	accounts map[string]*managedAccount
}

func newRegistry() *registry {
	return &registry{accounts: map[string]*managedAccount{}}
}

func (r *registry) get(key string) *managedAccount {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.accounts[key]
}

func (r *registry) all() []*managedAccount {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*managedAccount, 0, len(r.accounts))
	for _, ma := range r.accounts {
		out = append(out, ma)
	}
	return out
}

// getOrCreate returns the account for key, creating a stopped entry if new.
// Entries are never removed, so the session counter stays monotonic for the
// life of the process.
func (r *registry) getOrCreate(key, username string) *managedAccount {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ma, ok := r.accounts[key]; ok {
		return ma
	}
	ma := &managedAccount{
		key:      key,
		username: username,
		state:    stateStopped,
		since:    time.Now(),
	}
	r.accounts[key] = ma
	return ma
}

// loginReason maps a login response code to a reason string and whether
// the failure is fatal (retrying cannot help, so the run stops).
func loginReason(code int) (string, bool) {
	switch code {
	case -1:
		return "connection_error", false
	case 3:
		return "invalid_credentials", true
	case 4:
		return "already_logged_in", false
	case 5:
		return "client_outdated", true
	case 6:
		return "one_character_at_once", false
	case 7:
		return "login_attempts_exceeded", false
	case 8:
		return "session_rejected", false
	case 9:
		return "unable_to_login", false
	case 10:
		return "username_in_use", false
	case 11:
		return "account_temporarily_disabled", false
	case 12:
		return "account_permanently_disabled", true
	case 14:
		return "world_full", false
	case 15, 16, 17, 18, 20, 21, 22, 23, 24, 25:
		return "server_error", false
	case 100:
		return "unexpected_response", false
	default:
		return fmt.Sprintf("unknown_response_%d", code), false
	}
}

// runSession drives one login run of an account until logout is requested,
// a fatal login failure occurs, or the connection drops without autologin.
// It is the serve-mode counterpart of runClientLoop.
func (b *bot) runSession(ma *managedAccount) {
	defer close(ma.done)

	for {
		if ma.stopRequested() {
			ma.setState(stateStopped, "")
			return
		}

		c := &client{
			RSCPacket:        &RSCPacket{buf: make([]byte, 5000)},
			user:             ma.username,
			pass:             ma.password,
			world:            b.world,
			closed:           make(chan struct{}),
			closedLock:       &sync.Mutex{},
			packetChan:       make(chan *RSCPacket, 32),
			loading:          true,
			account:          ma.acct,
			localWorld:       newLocalWorld(),
			scriptKillSignal: make(chan struct{}, 1),
			scriptStopSignal: make(chan struct{}, 1),
			scriptDCSignal:   make(chan int, 1),
			calls:            make(chan *call),
		}
		c.onLoaded = func() {
			ma.mu.Lock()
			defer ma.mu.Unlock()
			if ma.stopRequested() {
				return // logout arrived while loading; stay stopping
			}
			ma.session++
			ma.client = c
			ma.setStateLocked(stateOnline, "")
		}

		// Logins are staggered process-wide (see loginWaiter).
		select {
		case b.loginWait <- struct{}{}:
		case <-ma.stop:
			ma.setState(stateStopped, "")
			return
		}

		success, retryWait, err := c.login()
		if err != nil {
			fmt.Printf("[%s] Login error: %s\n", ma.key, err)
		}
		if !success {
			if c.conn != nil {
				c.conn.Close()
			}
			reason, fatal := loginReason(c.loginResponse)
			if fatal {
				ma.setState(stateLoginFailed, reason)
				return
			}
			ma.setState(stateLoggingIn, "retrying: "+reason)
			if !ma.waitOrStop(retryWait) {
				ma.setState(stateStopped, "")
				return
			}
			continue
		}

		// Publish the live connection and check for a logout under the same
		// lock, so a logout request can't slip between the two.
		ma.mu.Lock()
		if ma.stopRequested() {
			ma.mu.Unlock()
			c.logout()
			c.conn.Close()
			ma.setState(stateStopped, "")
			return
		}
		ma.live = c
		ma.mu.Unlock()

		c.init()
		go c.readHandler()

		b.eventLoop(c)

		ma.mu.Lock()
		ma.client = nil
		ma.live = nil
		ma.mu.Unlock()

		if ma.stopRequested() {
			ma.setState(stateStopped, "")
			return
		}
		if !ma.autologin {
			ma.setState(stateStopped, "disconnected")
			return
		}

		ma.setState(stateDisconnected, "relogin pending")
		if !ma.waitOrStop(5 * time.Second) {
			ma.setState(stateStopped, "")
			return
		}
	}
}

// requestLogout asks the current run to end. If the account is online it
// sends the logout packet; the server may refuse while in combat, so the
// socket is closed after logoutGrace regardless.
func (s *server) requestLogout(ma *managedAccount) {
	ma.mu.Lock()
	switch ma.state {
	case stateStopped, stateLoginFailed, stateStopping:
		ma.mu.Unlock()
		return
	}
	select {
	case <-ma.stop:
	default:
		close(ma.stop)
	}
	ma.setStateLocked(stateStopping, "")
	ma.client = nil // no new calls once stopping
	c := ma.live
	ma.mu.Unlock()

	if c == nil {
		return
	}
	_, _ = s.callClient(c, func(c *client) (any, error) {
		c.logout()
		return nil, nil
	})
	go func() {
		t := time.NewTimer(logoutGrace)
		defer t.Stop()
		select {
		case <-c.closed:
		case <-t.C:
			fmt.Printf("[%s] Logout not acknowledged, closing connection\n", ma.key)
			c.close()
		}
	}()
}
