package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- the ring ------------------------------------------------------------------

func ids(evs []event) []uint64 {
	out := []uint64{}
	for _, ev := range evs {
		out = append(out, ev.id)
	}
	return out
}

func sameIDs(a []uint64, b ...uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRingSince(t *testing.T) {
	r := newEventRing(4)
	if got := r.since(0); got.lost || len(got.events) != 0 {
		t.Fatalf("empty ring: %+v", got)
	}
	for i := 0; i < 3; i++ {
		r.publish("death", struct{}{})
	}
	if got := r.since(0); got.lost || !sameIDs(ids(got.events), 1, 2, 3) {
		t.Fatalf("since 0: %+v", got)
	}
	if got := r.since(2); got.lost || !sameIDs(ids(got.events), 3) {
		t.Fatalf("since 2: %+v", got)
	}
	if got := r.since(3); got.lost || len(got.events) != 0 {
		t.Fatalf("caught up: %+v", got)
	}

	// Overflow: 7 events in a ring of 4 keeps 4..7.
	for i := 0; i < 4; i++ {
		r.publish("death", struct{}{})
	}
	if got := r.since(3); got.lost || !sameIDs(ids(got.events), 4, 5, 6, 7) {
		t.Fatalf("since 3, oldest still held: %+v", got)
	}
	if got := r.since(2); !got.lost || got.cursor != 3 || !sameIDs(ids(got.events), 4, 5, 6, 7) {
		t.Fatalf("since 2, event 3 overwritten: %+v", got)
	}
	if got := r.since(0); !got.lost || got.cursor != 3 {
		t.Fatalf("since 0 after overflow: %+v", got)
	}
	if got := r.since(99); !got.lost || got.cursor != 3 {
		t.Fatalf("cursor from the future: %+v", got)
	}
}

func TestRingWakesReaders(t *testing.T) {
	r := newEventRing(4)
	wake := r.since(0).wake
	r.serverTick()
	select {
	case <-wake:
	default:
		t.Fatal("serverTick did not wake readers")
	}
	got := r.since(0)
	if got.tick != 1 || len(got.events) != 0 {
		t.Fatalf("ticks must not enter the ring: %+v", got)
	}
	wake = got.wake
	r.publish("death", struct{}{})
	select {
	case <-wake:
	default:
		t.Fatal("publish did not wake readers")
	}
}

func TestPayloadIsCopiedAtPublish(t *testing.T) {
	c := &client{events: newEventRing(4)}
	n := &npc{id: 65, serverIndex: 412, x: 130, z: 512}
	c.emitNPC("npc_spawned", n)
	n.x = 999 // the game loop keeps mutating the live entity

	got := string(c.events.since(0).events[0].data)
	want := `{"npc":{"ref":"npc/412:id=65","id":65,"x":130,"z":512,"hp":0,"max_hp":0}}`
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestEmitIsNoOpOutsideServeMode(t *testing.T) {
	c := &client{}
	c.emit("death", struct{}{})
	c.emitNPC("npc_spawned", &npc{})
	c.emitServerTick()
}

func TestEntityRefs(t *testing.T) {
	cases := map[string]string{
		npcRef(&npc{serverIndex: 412, id: 65}):                    "npc/412:id=65",
		playerRef("Mike Gulak"):                                   "player/mike_gulak",
		playerRef(""):                                             "",
		objectRef(&object{id: 18, x: 130, z: 512}):                "object/18@130,512",
		wallObjectRef(&wallObject{id: 2, x: 130, z: 512, dir: 1}): "wall/2@130,512:1",
		groundItemRef(&groundItem{id: 10, x: 130, z: 512}):        "ground/10@130,512",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestParseEventFilter(t *testing.T) {
	all, err := parseEventFilter("")
	if err != nil || !all["chat_message"] || all["server_tick"] {
		t.Fatalf("default filter: %v %v", all, err)
	}
	some, err := parseEventFilter("chat_message, server_tick")
	if err != nil || !some["chat_message"] || !some["server_tick"] || some["death"] {
		t.Fatalf("explicit filter: %v %v", some, err)
	}
	_, err = parseEventFilter("chat_message,on_death,bogus")
	wantAPIError(t, err, http.StatusBadRequest, "bad_argument")
	if !strings.Contains(err.Error(), "bogus, on_death") {
		t.Fatalf("error should name the unknown events: %s", err)
	}
}

func TestParseLastEventID(t *testing.T) {
	r := newEventRing(8)
	r.publish("death", struct{}{})
	r.publish("death", struct{}{})

	cases := []struct {
		header string
		cursor uint64
		resync bool
	}{
		{"", 2, false},
		{"0", 0, false},
		{eventID(1), 1, false},
		{"otherepoch-1", 0, true},
	}
	for _, tc := range cases {
		cursor, resync, err := parseLastEventID(tc.header, r)
		if err != nil || cursor != tc.cursor || resync != tc.resync {
			t.Errorf("%q: got %d %t %v", tc.header, cursor, resync, err)
		}
	}
	for _, bad := range []string{"7", "x-y", eventEpoch + "-"} {
		_, _, err := parseLastEventID(bad, r)
		wantAPIError(t, err, http.StatusBadRequest, "bad_argument")
	}
}

// --- the stream over HTTP -------------------------------------------------------

type sseFrame struct {
	id, event, data string
	comment         bool
}

func (f sseFrame) json(t *testing.T) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := json.Unmarshal([]byte(f.data), &out); err != nil {
		t.Fatalf("%s data is not JSON: %q", f.event, f.data)
	}
	return out
}

type sseStream struct {
	t      *testing.T
	resp   *http.Response
	frames chan sseFrame
}

// openStream opens an event stream. lastEventID "" sends no header.
func (e *testEnv) openStream(path, lastEventID string) *sseStream {
	e.t.Helper()
	return openSSE(e.t, e.ts.URL+path, lastEventID)
}

// openSSE opens an event stream at a full URL and parses its frames.
// The live integration test uses it too.
func openSSE(t *testing.T, url, lastEventID string) *sseStream {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		resp.Body.Close()
		t.Fatalf("stream %s: %d %s", url, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	s := &sseStream{t: t, resp: resp, frames: make(chan sseFrame, 256)}
	go func() {
		defer close(s.frames)
		sc := bufio.NewScanner(resp.Body)
		var f sseFrame
		started := false
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if started {
					s.frames <- f
				}
				f, started = sseFrame{}, false
				continue
			}
			started = true
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "":
				f.comment = true
			case "id":
				f.id = value
			case "event":
				f.event = value
			case "data":
				f.data = value
			}
		}
	}()
	t.Cleanup(func() { resp.Body.Close() })
	return s
}

func (s *sseStream) close() { s.resp.Body.Close() }

// next returns the next frame that isn't a plain heartbeat comment.
func (s *sseStream) next() sseFrame {
	s.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-s.frames:
			if !ok {
				s.t.Fatal("stream ended")
			}
			if f.comment && f.id == "" {
				continue
			}
			return f
		case <-timeout:
			s.t.Fatal("no event within 5s")
		}
	}
}

// waitFor skips frames until one is the named event and satisfies match
// (nil matches anything). For streams too busy to predict, like a live
// game server's.
func (s *sseStream) waitFor(name string, within time.Duration, match func(map[string]any) bool) sseFrame {
	s.t.Helper()
	timeout := time.After(within)
	for {
		select {
		case f, ok := <-s.frames:
			if !ok {
				s.t.Fatalf("stream ended waiting for %s", name)
			}
			if f.event == name && (match == nil || match(f.json(s.t))) {
				return f
			}
		case <-timeout:
			s.t.Fatalf("no matching %s event within %s", name, within)
		}
	}
}

// expect returns the next frame, failing unless it is the named event.
func (s *sseStream) expect(name string) sseFrame {
	s.t.Helper()
	f := s.next()
	if f.event != name {
		s.t.Fatalf("got event %q (%s), want %q", f.event, f.data, name)
	}
	return f
}

func (s *sseStream) expectSession(state accountState, session int) sseFrame {
	s.t.Helper()
	f := s.expect(evSession)
	d := f.json(s.t)
	if d["state"] != string(state) || d["session"] != float64(session) {
		s.t.Fatalf("got session event %s, want state %s session %d", f.data, state, session)
	}
	return f
}

// ended waits for the server to close the stream.
func (s *sseStream) ended() {
	s.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-s.frames:
			if !ok {
				return
			}
		case <-timeout:
			s.t.Fatal("stream still open")
		}
	}
}

func idNum(t *testing.T, id string) uint64 {
	t.Helper()
	_, num, _ := strings.Cut(id, "-")
	n, err := strconv.ParseUint(num, 10, 64)
	if err != nil {
		t.Fatalf("bad event id %q", id)
	}
	return n
}

func TestEventsStreamAndResume(t *testing.T) {
	e := newTestEnv(t, "")
	fa := e.fakeOnline("Fellow Tst1", fakeOpts{})

	s := e.openStream("/v1/accounts/Fellow%20Tst1/events", "0")
	fa.c.emitMessage("chat_message", "hi there", "Some Guy")
	f := s.expect("chat_message")
	d := f.json(t)
	from, _ := d["from"].(map[string]any)
	if d["message"] != "hi there" || from["ref"] != "player/some_guy" || from["username"] != "Some Guy" {
		t.Fatalf("chat payload: %s", f.data)
	}
	s.close()

	// Missed while disconnected; resuming picks it up and nothing earlier.
	fa.c.emit("death", struct{}{})
	s = e.openStream("/v1/accounts/fellow_tst1/events", f.id)
	if g := s.expect("death"); idNum(t, g.id) != idNum(t, f.id)+1 {
		t.Fatalf("resumed at %s after %s", g.id, f.id)
	}

	// No Last-Event-ID: live tail only.
	s2 := e.openStream("/v1/accounts/fellow_tst1/events", "")
	fa.c.emit("server_message", map[string]any{"message": "new"})
	s2.expect("server_message")
}

func TestEventsResyncWhenOverwritten(t *testing.T) {
	setVar(t, &eventBufferSize, 4)
	e := newTestEnv(t, "")
	fa := e.fakeOnline("Fellow Tst1", fakeOpts{})

	for i := 0; i < 6; i++ {
		fa.c.emit("server_message", map[string]any{"message": strconv.Itoa(i)})
	}
	// Ring holds 3..6; a reader that last saw 1 has lost 2.
	s := e.openStream("/v1/accounts/fellow_tst1/events", eventID(1))
	r := s.expect(evResync)
	if r.json(t)["reason"] != "events_lost" || idNum(t, r.id) != 2 {
		t.Fatalf("resync: %+v", r)
	}
	if f := s.expect("server_message"); idNum(t, f.id) != 3 {
		t.Fatalf("after resync got id %s, want 3", f.id)
	}
	s.close()

	s = e.openStream("/v1/accounts/fellow_tst1/events", "oldprocess-4")
	if r := s.expect(evResync); r.json(t)["reason"] != "new_process" {
		t.Fatalf("resync: %s", r.data)
	}
}

func TestEventsFilterTicksAndSessions(t *testing.T) {
	e := newTestEnv(t, "")
	fa := e.fakeOnline("Fellow Tst1", fakeOpts{})

	s := e.openStream("/v1/accounts/fellow_tst1/events?events=chat_message,server_tick", "")
	fa.c.emit("server_message", map[string]any{"message": "filtered out"})
	fa.c.emitServerTick()
	tick := s.expect(evTick)
	if tick.id != "" || tick.json(t)["tick"] != float64(1) {
		t.Fatalf("tick must carry no id: %+v", tick)
	}
	fa.ma.setState(stateDisconnected, "relogin pending") // session is never filtered
	s.expectSession(stateDisconnected, 1)
	fa.c.emitMessage("chat_message", "kept", "Some Guy")
	s.expect("chat_message")

	// The default stream has no ticks.
	s2 := e.openStream("/v1/accounts/fellow_tst1/events", "")
	fa.c.emitServerTick()
	fa.c.emit("death", struct{}{})
	s2.expect("death")
}

func TestEventsHeartbeatMovesFilteredCursor(t *testing.T) {
	setVar(t, &sseHeartbeat, 30*time.Millisecond)
	e := newTestEnv(t, "")
	fa := e.fakeOnline("Fellow Tst1", fakeOpts{})

	s := e.openStream("/v1/accounts/fellow_tst1/events?events=chat_message", "")
	fa.c.emit("server_message", map[string]any{"message": "skipped"})
	f := s.next()
	if !f.comment || idNum(t, f.id) != fa.ma.events.last() {
		t.Fatalf("want a heartbeat carrying the skipped id, got %+v", f)
	}
}

func TestEventsRequestErrors(t *testing.T) {
	e := newTestEnv(t, "")
	e.fakeOnline("Fellow Tst1", fakeOpts{})

	e.do("GET", "/v1/accounts/nobody/events", "").want(t, 404, "unknown_account")
	e.do("GET", "/v1/accounts/fellow_tst1/events?events=bogus", "").want(t, 400, "bad_argument")
	e.do("GET", "/v1/accounts/fellow_tst1/events", "", "Last-Event-ID", "garbage").want(t, 400, "bad_argument")
	e.do("POST", "/v1/accounts/fellow_tst1/events", "").want(t, 404, "not_found")
}

// --- end to end over the fake game server --------------------------------------

// TestSessionLifecycleOverFakeGame runs the real session goroutine, event
// loop and packet handlers through login, game events, a call, logout,
// relogin and a SIGTERM-style shutdown that the brain answers.
func TestSessionLifecycleOverFakeGame(t *testing.T) {
	g := startFakeGame(t)
	e := newTestEnv(t, "")
	grantLogins(t, e.s.bot)

	e.do("POST", "/v1/accounts/Fellow%20Tst1/login", `{"password":"pw","autologin":false}`).want(t, 202, "")
	s := e.openStream("/v1/accounts/fellow_tst1/events", "0")
	s.expectSession(stateLoggingIn, 0)
	g.waitLogin()
	if names := g.loginNames(); names[0] != "Fellow_Tst1" {
		t.Fatalf("fake game saw username %q", names[0])
	}
	s.expectSession(stateLoggingIn, 1) // connected, loading
	g.worldLoaded()
	s.expectSession(stateOnline, 1)

	g.serverMessage(4, "hello bot", "Some Guy")
	if d := s.expect("chat_message").json(t); d["message"] != "hello bot" {
		t.Fatalf("chat: %v", d)
	}
	g.serverMessage(6, "", "Trader Joe")
	if d := s.expect("trade_request").json(t); d["from"].(map[string]any)["ref"] != "player/trader_joe" {
		t.Fatalf("trade: %v", d)
	}
	g.send(114, u16(150), u16(375)) // fatigue
	if d := s.expect("fatigue_update").json(t); d["fatigue"] != float64(150) || d["accurate_fatigue"] != float64(50) {
		t.Fatalf("fatigue: %v", d)
	}
	g.send(83) // death
	s.expect("death")
	g.send(245, u8(2), str("Yes"), str("No")) // option menu
	if d := s.expect("option_menu").json(t); len(d["options"].([]any)) != 2 {
		t.Fatalf("option menu: %v", d)
	}

	r := e.do("GET", "/v1/accounts/fellow_tst1/fn/get_fatigue", "")
	r.want(t, 200, "")
	if r.body["session"] != float64(1) {
		t.Fatalf("call: %v", r.body)
	}

	e.do("POST", "/v1/accounts/fellow_tst1/logout", "").want(t, 202, "")
	s.expectSession(stateStopping, 1)
	if op := g.nextPacket(); op != 102 {
		t.Fatalf("logout sent opcode %d", op)
	}
	s.expectSession(stateStopped, 1)

	// Relogin: same stream, numbering continues, session 2.
	e.do("POST", "/v1/accounts/fellow_tst1/login", `{"password":"pw","autologin":false}`).want(t, 202, "")
	s.expectSession(stateLoggingIn, 1)
	g.waitLogin()
	s.expectSession(stateLoggingIn, 2)
	g.worldLoaded()
	s.expectSession(stateOnline, 2)

	// SIGTERM: kill_signal reaches the listening brain, which logs out;
	// the shutdown then completes well inside killGrace.
	done := make(chan struct{})
	go func() { e.s.shutdown(e.ts.Config); close(done) }()
	if d := s.expect("kill_signal").json(t); d["seconds"] != float64(15) {
		t.Fatalf("kill_signal: %v", d)
	}
	e.do("POST", "/v1/accounts/fellow_tst1/login", `{"password":"pw"}`).want(t, 503, "shutting_down")
	e.do("POST", "/v1/accounts/fellow_tst1/logout", "").want(t, 202, "")
	s.expectSession(stateStopping, 2)
	s.expectSession(stateStopped, 2)
	s.ended()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish after the brain logged out")
	}
}

func TestFatalLoginOverFakeGame(t *testing.T) {
	g := startFakeGame(t)
	g.setLoginCode(3) // invalid credentials
	e := newTestEnv(t, "")
	grantLogins(t, e.s.bot)

	e.do("POST", "/v1/accounts/fellow_tst1/login", `{"password":"wrong"}`).want(t, 202, "")
	s := e.openStream("/v1/accounts/fellow_tst1/events", "0")
	s.expectSession(stateLoggingIn, 0)
	f := s.expectSession(stateLoginFailed, 0)
	if f.json(t)["reason"] != "invalid_credentials" {
		t.Fatalf("reason: %s", f.data)
	}
}
