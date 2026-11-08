package main

// Serve mode (-serve): plutonium as an HTTP server for the fellowship brain.
//
// JSON REST for calls; no Python anywhere on this path. Accounts are created
// and logged in over HTTP, addressed by canonical key (see accountKey), and
// script functions are served under their verbatim names at
// /v1/accounts/{account}/fn/{function}: GET for queries, POST for actions.
//
// Errors the brain can recover from are 4xx. 5xx is reserved for adapter
// bugs (500), unknown game-server failures (502) and an unresponsive event
// loop (504).

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	apiTokenEnv      = "PLUTONIUM_API_TOKEN"
	maxBodyBytes     = 64 << 10
	shutdownGrace    = 15 * time.Second
	registerSpacing  = 2 * time.Second
	registerCooldown = 5 * time.Minute
)

type server struct {
	bot      *bot
	accounts *registry
	token    string

	registerMu   sync.Mutex
	lastRegister time.Time
}

// apiError is the single error envelope:
// {"error":{"code":"…","message":"…", …extra}}
type apiError struct {
	status  int
	code    string
	message string
	extra   map[string]any
	header  map[string]string
}

func (e *apiError) Error() string { return e.code + ": " + e.message }

func offlineError(ma *managedAccount) *apiError {
	v := ma.view()
	return &apiError{
		status:  http.StatusConflict,
		code:    "account_offline",
		message: "account is not online",
		extra:   map[string]any{"state": v.State, "reason": v.Reason},
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		ae = &apiError{status: http.StatusInternalServerError, code: "internal", message: err.Error()}
	}
	body := map[string]any{"code": ae.code, "message": ae.message}
	for k, v := range ae.extra {
		body[k] = v
	}
	for k, v := range ae.header {
		w.Header().Set(k, v)
	}
	writeJSON(w, ae.status, map[string]any{"error": body})
}

// decodeBody reads a JSON object body into dst. An empty body is allowed.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		return &apiError{status: http.StatusBadRequest, code: "bad_json", message: err.Error()}
	}
	return nil
}

// runServer serves until SIGINT/SIGTERM arrives on sigs, then logs every
// account out gracefully before returning.
func runServer(b *bot, addr string, sigs <-chan os.Signal) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Printf("[BOT] Bad -serve address %q: %s\n", addr, err)
		os.Exit(1)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		fmt.Printf("[BOT] -serve must bind to a loopback address, got %q\n", host)
		os.Exit(1)
	}

	s := newServer(b, os.Getenv(apiTokenEnv))

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Printf("[BOT] Listen on %s: %s\n", addr, err)
		os.Exit(1)
	}
	fmt.Printf("[BOT] Serving on http://%s (token required: %t)\n", ln.Addr(), s.token != "")

	go func() {
		if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Printf("[BOT] HTTP server error: %s\n", err)
		}
	}()

	<-sigs
	fmt.Println("[BOT] Received kill signal, logging accounts out")
	s.shutdown(httpServer)
}

func newServer(b *bot, token string) *server {
	return &server{bot: b, accounts: newRegistry(), token: token}
}

// handler returns the full HTTP handler: routes behind the token check.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/accounts", s.handleRegister)
	mux.HandleFunc("GET /v1/accounts", s.handleListAccounts)
	mux.HandleFunc("GET /v1/accounts/{account}", s.handleGetAccount)
	mux.HandleFunc("POST /v1/accounts/{account}/login", s.handleLogin)
	mux.HandleFunc("POST /v1/accounts/{account}/logout", s.handleLogout)
	mux.HandleFunc("GET /v1/accounts/{account}/fn/{function}", s.handleFn)
	mux.HandleFunc("POST /v1/accounts/{account}/fn/{function}", s.handleFn)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, &apiError{status: http.StatusNotFound, code: "not_found", message: "no such route"})
	})
	return s.auth(mux)
}

func badUsername() *apiError {
	return &apiError{
		status:  http.StatusBadRequest,
		code:    "bad_username",
		message: "username must have 1-12 characters once spaces and symbols at either end are removed",
	}
}

func (s *server) shutdown(httpServer *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	// Stop taking new requests first, so nothing logs back in mid-shutdown.
	_ = httpServer.Shutdown(ctx)

	var runs []chan struct{}
	for _, ma := range s.accounts.all() {
		ma.mu.Lock()
		done := ma.done
		ma.mu.Unlock()
		if done != nil {
			runs = append(runs, done)
		}
		s.requestLogout(ma)
	}
	for _, done := range runs {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	close(s.bot.closing)
	fmt.Println("[BOT] Shut down")
}

// auth enforces the optional bearer token from PLUTONIUM_API_TOKEN.
func (s *server) auth(next http.Handler) http.Handler {
	if s.token == "" {
		return next
	}
	want := []byte("Bearer " + s.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			writeError(w, &apiError{status: http.StatusUnauthorized, code: "unauthorized", message: "missing or wrong bearer token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// accountFromPath resolves {account} to a known account. Any spelling of the
// username is accepted; it is canonicalised before lookup.
func (s *server) accountFromPath(r *http.Request) (*managedAccount, error) {
	key := accountKey(r.PathValue("account"))
	if key == "" {
		return nil, badUsername()
	}
	ma := s.accounts.get(key)
	if ma == nil {
		return nil, &apiError{status: http.StatusNotFound, code: "unknown_account", message: "no account " + key + " in this process; log it in first", extra: map[string]any{"key": key}}
	}
	return ma, nil
}

// --- registration ---------------------------------------------------------

type registerRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Email    string `json:"email"`
}

func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.Username == "" || req.Password == "" {
		writeError(w, &apiError{status: http.StatusBadRequest, code: "bad_argument", message: "username and password are required"})
		return
	}
	key := accountKey(req.Username)
	if key == "" {
		writeError(w, badUsername())
		return
	}

	// Registrations are serialised and spaced, as the -r path does.
	s.registerMu.Lock()
	if wait := registerSpacing - time.Since(s.lastRegister); wait > 0 {
		time.Sleep(wait)
	}
	code, err := registerAccount(req.Username, req.Password, req.Email)
	s.lastRegister = time.Now()
	s.registerMu.Unlock()

	if err != nil {
		writeError(w, &apiError{status: http.StatusBadGateway, code: "game_server_error", message: err.Error()})
		return
	}

	view := map[string]any{"key": key, "username": req.Username}
	if err := registrationError(code, view); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// registrationError maps a game-server registration response code to the
// API error for it, or nil for 0 (created).
func registrationError(code byte, view map[string]any) *apiError {
	switch code {
	case 0:
		return nil
	case 2:
		return &apiError{status: http.StatusConflict, code: "username_taken", message: "username already taken", extra: view}
	case 3:
		return &apiError{status: http.StatusConflict, code: "email_in_use", message: "email already in use"}
	case 4:
		return &apiError{status: http.StatusForbidden, code: "registration_disabled", message: "registration is disabled on the game server"}
	case 5:
		return &apiError{
			status:  http.StatusTooManyRequests,
			code:    "registered_too_recently",
			message: "the game server rate-limits registration",
			header:  map[string]string{"Retry-After": strconv.Itoa(int(registerCooldown.Seconds()))},
		}
	case 6:
		return &apiError{status: http.StatusBadRequest, code: "bad_email", message: "invalid email address"}
	case 7:
		return &apiError{status: http.StatusBadRequest, code: "bad_username", message: "username must be 2-12 characters long"}
	case 8:
		return &apiError{status: http.StatusBadRequest, code: "bad_username", message: "invalid username"}
	default:
		return &apiError{status: http.StatusBadGateway, code: "registration_failed", message: fmt.Sprintf("game server registration response %d", code)}
	}
}

// --- accounts and lifecycle -------------------------------------------------

func (s *server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	all := s.accounts.all()
	views := make([]accountView, 0, len(all))
	for _, ma := range all {
		views = append(views, ma.view())
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Key < views[j].Key })
	writeJSON(w, http.StatusOK, map[string]any{"accounts": views})
}

func (s *server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	ma, err := s.accountFromPath(r)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ma.view())
}

type loginRequest struct {
	Password  string `json:"password"`
	Autologin *bool  `json:"autologin"`
}

// handleLogin starts a login run and answers 202 straight away; logins are
// staggered and can take a while, so the outcome is read from the account's
// state (and, with FELLOW-4, its session events).
func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("account")
	key := accountKey(username)
	if key == "" {
		writeError(w, badUsername())
		return
	}
	var req loginRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.Password == "" {
		writeError(w, &apiError{status: http.StatusBadRequest, code: "bad_argument", message: "password is required"})
		return
	}

	ma := s.accounts.getOrCreate(key, username)
	ma.mu.Lock()
	switch ma.state {
	case stateStopped, stateLoginFailed:
	default:
		state := ma.state
		ma.mu.Unlock()
		writeError(w, &apiError{status: http.StatusConflict, code: "already_running", message: "account already has a login run; log it out first", extra: map[string]any{"state": state}})
		return
	}
	ma.username = username
	ma.password = req.Password
	ma.autologin = req.Autologin == nil || *req.Autologin
	ma.acct = &account{
		username:    username,
		password:    req.Password,
		autologin:   ma.autologin,
		enabled:     true,
		debug:       true,
		progressDur: noProgressDurSet,
	}
	ma.stop = make(chan struct{})
	ma.done = make(chan struct{})
	ma.setStateLocked(stateLoggingIn, "")
	ma.mu.Unlock()

	go s.bot.runSession(ma)

	writeJSON(w, http.StatusAccepted, ma.view())
}

// handleLogout ends the account's run. It is idempotent: logging out an
// account that is already stopped succeeds.
func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	ma, err := s.accountFromPath(r)
	if err != nil {
		writeError(w, err)
		return
	}
	s.requestLogout(ma)
	writeJSON(w, http.StatusAccepted, ma.view())
}

// --- functions ----------------------------------------------------------------

func (s *server) handleFn(w http.ResponseWriter, r *http.Request) {
	ma, err := s.accountFromPath(r)
	if err != nil {
		writeError(w, err)
		return
	}

	name := r.PathValue("function")
	spec, ok := functions[name]
	if !ok {
		writeError(w, &apiError{status: http.StatusNotFound, code: "unknown_function", message: "no function " + name})
		return
	}

	var args fnArgs
	switch {
	case spec.kind == fnQuery && r.Method == http.MethodGet:
		args.query = r.URL.Query()
	case spec.kind == fnAction && r.Method == http.MethodPost:
		args.body = map[string]json.RawMessage{}
		if err := decodeBody(w, r, &args.body); err != nil {
			writeError(w, err)
			return
		}
	default:
		allow := http.MethodGet
		if spec.kind == fnAction {
			allow = http.MethodPost
		}
		writeError(w, &apiError{
			status:  http.StatusMethodNotAllowed,
			code:    "wrong_method",
			message: name + " must be called with " + allow,
			header:  map[string]string{"Allow": allow},
		})
		return
	}
	if err := spec.validate(args); err != nil {
		writeError(w, err)
		return
	}

	ma.mu.Lock()
	c, state, session := ma.client, ma.state, ma.session
	ma.mu.Unlock()
	if c == nil || state != stateOnline {
		writeError(w, offlineError(ma))
		return
	}

	result, err := s.callClient(c, func(c *client) (any, error) { return spec.call(c, args) })
	if errors.Is(err, errAccountOffline) {
		writeError(w, offlineError(ma))
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result, "session": session})
}
