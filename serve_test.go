package main

// HTTP component tests: the real handlers, served in memory by httptest,
// against accounts whose event loop is faked. The game socket is a
// net.Pipe, so tests can read the exact packets a call sends.

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type testEnv struct {
	t  *testing.T
	s  *server
	ts *httptest.Server
}

func newTestEnv(t *testing.T, token string) *testEnv {
	t.Helper()
	b := &bot{closing: make(chan struct{}), loginWait: make(chan struct{})}
	s := newServer(b, token)
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return &testEnv{t: t, s: s, ts: ts}
}

type response struct {
	status int
	header http.Header
	body   map[string]any
}

// errCode returns error.code from an error envelope, or "".
func (r response) errCode() string {
	e, _ := r.body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func (e *testEnv) do(method, path, body string, header ...string) response {
	e.t.Helper()
	resp, raw := e.raw(method, path, body, header...)
	out := response{status: resp.StatusCode, header: resp.Header, body: map[string]any{}}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.body); err != nil {
			e.t.Fatalf("%s %s: response is not a JSON object: %s", method, path, raw)
		}
	}
	return out
}

// raw sends a request and returns the response with its body exactly as sent.
func (e *testEnv) raw(method, path, body string, header ...string) (*http.Response, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

// want fails unless the response has the given status and (if non-empty)
// error code.
func (r response) want(t *testing.T, status int, code string) {
	t.Helper()
	if r.status != status || r.errCode() != code {
		t.Fatalf("got %d %q %v, want %d %q", r.status, r.errCode(), r.body, status, code)
	}
}

// fakeAccount is an account marked online whose *client has a fake event
// loop and a piped socket.
type fakeAccount struct {
	ma      *managedAccount
	c       *client
	packets chan []byte // frames the client sent: opcode, then payload
}

type fakeOpts struct {
	noLoop bool // never serve calls, to provoke timeouts
	setup  func(c *client)
}

func (e *testEnv) fakeOnline(username string, opts fakeOpts) *fakeAccount {
	e.t.Helper()
	key := accountKey(username)
	local, remote := net.Pipe()
	c := &client{
		RSCPacket:  &RSCPacket{buf: make([]byte, 5000)},
		user:       key,
		conn:       local,
		closed:     make(chan struct{}),
		closedLock: &sync.Mutex{},
		calls:      make(chan *call),
		account:    &account{},
	}
	if opts.setup != nil {
		opts.setup(c)
	}

	fa := &fakeAccount{c: c, packets: make(chan []byte, 16)}

	// The fake game server: read framed packets the client writes.
	go func() {
		for {
			lenBuf := make([]byte, 2)
			if _, err := io.ReadFull(remote, lenBuf); err != nil {
				return
			}
			frame := make([]byte, int(lenBuf[0])<<8|int(lenBuf[1]))
			if _, err := io.ReadFull(remote, frame); err != nil {
				return
			}
			fa.packets <- frame
		}
	}()

	// The fake event loop: serve calls until closed, like eventLoop does.
	if !opts.noLoop {
		go func() {
			for {
				select {
				case cl := <-c.calls:
					cl.invoke(c)
				case <-c.closed:
					return
				}
			}
		}()
	}

	ma := e.s.accounts.getOrCreate(key, username)
	c.events = ma.events
	ma.mu.Lock()
	ma.state = stateOnline
	ma.session = 1
	ma.client = c
	ma.live = c
	ma.stop = make(chan struct{})
	ma.done = make(chan struct{})
	ma.mu.Unlock()
	fa.ma = ma

	e.t.Cleanup(func() {
		c.close()
		local.Close()
		remote.Close()
	})
	return fa
}

// nextPacket waits for the next frame the client sends and returns its opcode.
func (fa *fakeAccount) nextPacket(t *testing.T) byte {
	t.Helper()
	select {
	case frame := <-fa.packets:
		return frame[0]
	case <-time.After(2 * time.Second):
		t.Fatal("no packet sent")
		return 0
	}
}

func (fa *fakeAccount) noPacket(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case frame := <-fa.packets:
		t.Fatalf("unexpected packet with opcode %d", frame[0])
	case <-time.After(within):
	}
}

// setVar sets a package variable for one test and restores it afterwards.
func setVar[T any](t *testing.T, v *T, val T) {
	old := *v
	*v = val
	t.Cleanup(func() { *v = old })
}

// waitUntil polls until cond holds, for effects of a packet the event loop
// handles after the test sent it.
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not met within 5s")
}

// --- calls ------------------------------------------------------------------

func TestQueryReturnsResultAndSession(t *testing.T) {
	e := newTestEnv(t, "")
	e.fakeOnline("Fellow Tst1", fakeOpts{setup: func(c *client) { c.x, c.z = 216, 744 }})

	r := e.do("GET", "/v1/accounts/fellow_tst1/fn/get_x", "")
	r.want(t, 200, "")
	if r.body["result"] != float64(216) || r.body["session"] != float64(1) {
		t.Fatalf("got %v", r.body)
	}
	r = e.do("GET", "/v1/accounts/fellow_tst1/fn/get_z", "")
	if r.body["result"] != float64(744) {
		t.Fatalf("got %v", r.body)
	}
}

func TestAccountAddressedByAnySpelling(t *testing.T) {
	e := newTestEnv(t, "")
	e.fakeOnline("Fellow Tst1", fakeOpts{})
	for _, path := range []string{"FELLOW%20TST1", "fellow-tst1", "__Fellow_Tst1__"} {
		e.do("GET", "/v1/accounts/"+path+"/fn/get_x", "").want(t, 200, "")
	}
}

func TestActionSendsPacket(t *testing.T) {
	e := newTestEnv(t, "")
	fa := e.fakeOnline("fellow", fakeOpts{})

	e.do("POST", "/v1/accounts/fellow/fn/send_chat_message", `{"message":"hello"}`).want(t, 200, "")
	if op := fa.nextPacket(t); op != 216 {
		t.Fatalf("sent opcode %d, want 216 (chat)", op)
	}
}

// withScene gives a fake client a tree and a stump in view, and an axe and
// two logs in its inventory.
func withScene(c *client) {
	c.x, c.z = 170, 641
	for i, o := range []*object{{id: 1, x: 170, z: 640}, {id: 4, x: 172, z: 640}} {
		c.objects[i] = o
	}
	c.objectCount = 2
	// A closed door (2) on the north edge of the bot's tile, and an open doorframe (1) beside it.
	for i, w := range []*wallObject{{id: 2, x: 170, z: 641, dir: 0}, {id: 1, x: 171, z: 641, dir: 0}} {
		c.wallObjects[i] = w
	}
	c.wallObjectCount = 2
	for i, it := range []*item{{id: 87, amount: 1, equipped: true}, {id: 14, amount: 1}, {id: 14, amount: 1}} {
		it.slot = i
		c.inventory[i] = it
	}
	c.inventoryCount = 3
}

func TestObjectsAndInventoryQueries(t *testing.T) {
	e := newTestEnv(t, "")
	e.fakeOnline("fellow", fakeOpts{setup: withScene})

	r := e.do("GET", "/v1/accounts/fellow/fn/get_objects", "")
	r.want(t, 200, "")
	objects := r.body["result"].([]any)
	if len(objects) != 2 || objects[0].(map[string]any)["ref"] != "object/1@170,640" || objects[1].(map[string]any)["id"] != float64(4) {
		t.Fatalf("get_objects: %v", r.body)
	}

	r = e.do("GET", "/v1/accounts/fellow/fn/get_inventory_items", "")
	r.want(t, 200, "")
	items := r.body["result"].([]any)
	axe := items[0].(map[string]any)
	if len(items) != 3 || axe["ref"] != "inv/0" || axe["id"] != float64(87) || axe["equipped"] != true || items[2].(map[string]any)["slot"] != float64(2) {
		t.Fatalf("get_inventory_items: %v", r.body)
	}

	r = e.do("GET", "/v1/accounts/fellow/fn/get_empty_slots", "")
	if r.body["result"] != float64(27) {
		t.Fatalf("get_empty_slots: %v", r.body)
	}
}

func TestWallObjectsQuery(t *testing.T) {
	e := newTestEnv(t, "")
	e.fakeOnline("fellow", fakeOpts{setup: withScene})

	r := e.do("GET", "/v1/accounts/fellow/fn/get_wall_objects", "")
	r.want(t, 200, "")
	walls := r.body["result"].([]any)
	door := walls[0].(map[string]any)
	if len(walls) != 2 || door["ref"] != "wall/2@170,641:0" || door["id"] != float64(2) || door["dir"] != float64(0) {
		t.Fatalf("get_wall_objects: %v", r.body)
	}
}

func TestEmptyListsAreArraysNotNull(t *testing.T) {
	e := newTestEnv(t, "")
	e.fakeOnline("fellow", fakeOpts{})
	for _, fn := range []string{"get_objects", "get_wall_objects", "get_inventory_items"} {
		r := e.do("GET", "/v1/accounts/fellow/fn/"+fn, "")
		if list, ok := r.body["result"].([]any); !ok || len(list) != 0 {
			t.Fatalf("%s: %v", fn, r.body)
		}
	}
}

func TestAtObjectThatIsGoneIs410WithoutAPacket(t *testing.T) {
	e := newTestEnv(t, "")
	fa := e.fakeOnline("fellow", fakeOpts{setup: withScene})

	for _, ref := range []string{
		"object/1@172,640", // a stump stands there now
		"object/1@180,640", // nothing there
	} {
		r := e.do("POST", "/v1/accounts/fellow/fn/at_object", `{"object":"`+ref+`"}`)
		r.want(t, 410, "entity_gone")
		if got := r.body["error"].(map[string]any)["ref"]; got != ref {
			t.Fatalf("error ref %v, want %s", got, ref)
		}
	}
	e.do("POST", "/v1/accounts/fellow/fn/at_object", `{"object":"tree"}`).want(t, 400, "bad_argument")
	fa.noPacket(t, 100*time.Millisecond)
}

func TestAtWallObjectThatIsGoneIs410WithoutAPacket(t *testing.T) {
	e := newTestEnv(t, "")
	fa := e.fakeOnline("fellow", fakeOpts{setup: withScene})

	for _, ref := range []string{
		"wall/2@171,641:0", // a doorframe stands there, not a door
		"wall/2@170,641:1", // the door is on another edge of that tile
		"wall/2@180,641:0", // nothing there
	} {
		r := e.do("POST", "/v1/accounts/fellow/fn/at_wall_object", `{"wall_object":"`+ref+`"}`)
		r.want(t, 410, "entity_gone")
		if got := r.body["error"].(map[string]any)["ref"]; got != ref {
			t.Fatalf("error ref %v, want %s", got, ref)
		}
	}
	e.do("POST", "/v1/accounts/fellow/fn/at_wall_object", `{"wall_object":"object/2@170,641"}`).want(t, 400, "bad_argument")
	e.do("POST", "/v1/accounts/fellow/fn/at_object2", `{"object":"object/1@180,640"}`).want(t, 410, "entity_gone")
	fa.noPacket(t, 100*time.Millisecond)
}

// withBanker gives a fake client a banker (95) at server index 12 in view,
// and the banker's option menu showing.
func withBanker(c *client) {
	c.x, c.z = 220, 634
	c.npcs[0] = &npc{id: 95, serverIndex: 12, x: 220, z: 636, currentHP: 1, maxHP: 1}
	c.npcCount = 1
	c.optionMenu[0] = "I'd like to access my bank account please"
	c.optionMenu[1] = "What is this place?"
	c.optionMenuCount = 2
	c.optionMenuVisible = true
}

func TestNPCAndOptionMenuQueries(t *testing.T) {
	e := newTestEnv(t, "")
	e.fakeOnline("fellow", fakeOpts{setup: withBanker})

	r := e.do("GET", "/v1/accounts/fellow/fn/get_npcs", "")
	r.want(t, 200, "")
	npcs := r.body["result"].([]any)
	if len(npcs) != 1 || npcs[0].(map[string]any)["ref"] != "npc/12:id=95" || npcs[0].(map[string]any)["z"] != float64(636) {
		t.Fatalf("get_npcs: %v", r.body)
	}

	if r := e.do("GET", "/v1/accounts/fellow/fn/is_option_menu", ""); r.body["result"] != true {
		t.Fatalf("is_option_menu: %v", r.body)
	}
	r = e.do("GET", "/v1/accounts/fellow/fn/get_option_menu", "")
	options := r.body["result"].([]any)
	if len(options) != 2 || options[0] != "I'd like to access my bank account please" {
		t.Fatalf("get_option_menu: %v", r.body)
	}
	if r := e.do("GET", "/v1/accounts/fellow/fn/is_bank_open", ""); r.body["result"] != false {
		t.Fatalf("is_bank_open: %v", r.body)
	}
}

func TestEmptyNPCsAndMenuAreArraysNotNull(t *testing.T) {
	e := newTestEnv(t, "")
	e.fakeOnline("fellow", fakeOpts{})
	for _, fn := range []string{"get_npcs", "get_option_menu"} {
		r := e.do("GET", "/v1/accounts/fellow/fn/"+fn, "")
		if list, ok := r.body["result"].([]any); !ok || len(list) != 0 {
			t.Fatalf("%s: %v", fn, r.body)
		}
	}
}

// talk_to_npc's success path walks to the NPC first, and the pathfinder needs
// the landscape archive these tests don't load (at_object is the same); the
// live run covers it.
func TestBankingActionsSendPackets(t *testing.T) {
	e := newTestEnv(t, "")
	fa := e.fakeOnline("fellow", fakeOpts{setup: withBanker})

	e.do("POST", "/v1/accounts/fellow/fn/answer", `{"index":0}`).want(t, 200, "")
	if op := fa.nextPacket(t); op != 116 {
		t.Fatalf("answer sent opcode %d, want 116", op)
	}
	if r := e.do("GET", "/v1/accounts/fellow/fn/is_option_menu", ""); r.body["result"] != false {
		t.Fatalf("menu still showing after answer: %v", r.body)
	}

	e.do("POST", "/v1/accounts/fellow/fn/deposit", `{"id":14,"amount":27}`).want(t, 200, "")
	if op := fa.nextPacket(t); op != 23 {
		t.Fatalf("deposit sent opcode %d, want 23", op)
	}
	e.do("POST", "/v1/accounts/fellow/fn/close_bank", `{}`).want(t, 200, "")
	if op := fa.nextPacket(t); op != 212 {
		t.Fatalf("close_bank sent opcode %d, want 212", op)
	}
}

func TestBankingArgumentsAndGoneNPC(t *testing.T) {
	e := newTestEnv(t, "")
	fa := e.fakeOnline("fellow", fakeOpts{setup: withBanker})

	for _, ref := range []string{
		"npc/12:id=65", // another type of NPC holds that index now
		"npc/13:id=95", // nothing at that index
	} {
		r := e.do("POST", "/v1/accounts/fellow/fn/talk_to_npc", `{"npc":"`+ref+`"}`)
		r.want(t, 410, "entity_gone")
		if got := r.body["error"].(map[string]any)["ref"]; got != ref {
			t.Fatalf("error ref %v, want %s", got, ref)
		}
	}
	for _, body := range []string{
		`{"npc":"banker"}`, `{"npc":"npc/12"}`, `{"npc":"npc/012:id=95"}`, `{"npc":"object/1@170,640"}`,
	} {
		e.do("POST", "/v1/accounts/fellow/fn/talk_to_npc", body).want(t, 400, "bad_argument")
	}
	e.do("POST", "/v1/accounts/fellow/fn/answer", `{"index":-1}`).want(t, 400, "bad_argument")
	e.do("POST", "/v1/accounts/fellow/fn/answer", `{"index":20}`).want(t, 400, "bad_argument")
	e.do("POST", "/v1/accounts/fellow/fn/answer", `{"index":2}`).want(t, 400, "bad_argument") // the menu has two options
	e.do("POST", "/v1/accounts/fellow/fn/deposit", `{"id":14,"amount":0}`).want(t, 400, "bad_argument")
	e.do("POST", "/v1/accounts/fellow/fn/deposit", `{"id":70000,"amount":1}`).want(t, 400, "bad_argument")
	e.do("POST", "/v1/accounts/fellow/fn/deposit", `{"id":14}`).want(t, 400, "bad_argument")
	fa.noPacket(t, 100*time.Millisecond)
}

// withSleepingBag gives a fake client an axe and a sleeping bag (1263).
func withSleepingBag(c *client) {
	for i, it := range []*item{{id: 87, amount: 1, equipped: true}, {id: 1263, amount: 1}} {
		it.slot = i
		c.inventory[i] = it
	}
	c.inventoryCount = 2
}

func TestUseSleepingBagSendsThePacketAndAnswersTrue(t *testing.T) {
	e := newTestEnv(t, "")
	fa := e.fakeOnline("fellow", fakeOpts{setup: withSleepingBag})

	r := e.do("POST", "/v1/accounts/fellow/fn/use_sleeping_bag", `{}`)
	r.want(t, 200, "")
	if r.body["result"] != true {
		t.Fatalf("use_sleeping_bag: %v", r.body)
	}
	if op := fa.nextPacket(t); op != 90 {
		t.Fatalf("use_sleeping_bag sent opcode %d, want 90", op)
	}
}

func TestUseSleepingBagWithNoBagAnswersFalseAndSendsNothing(t *testing.T) {
	e := newTestEnv(t, "")
	fa := e.fakeOnline("fellow", fakeOpts{setup: withScene}) // an axe and logs, no bag

	r := e.do("POST", "/v1/accounts/fellow/fn/use_sleeping_bag", `{}`)
	r.want(t, 200, "")
	if r.body["result"] != false {
		t.Fatalf("use_sleeping_bag with no bag: %v", r.body)
	}
	fa.noPacket(t, 100*time.Millisecond)
	e.do("POST", "/v1/accounts/fellow/fn/use_sleeping_bag", `{"slot":1}`).want(t, 400, "bad_argument")
}

func TestIsSleepingFollowsTheSleepScreen(t *testing.T) {
	e := newTestEnv(t, "")
	e.fakeOnline("fellow", fakeOpts{})
	if r := e.do("GET", "/v1/accounts/fellow/fn/is_sleeping", ""); r.body["result"] != false {
		t.Fatalf("is_sleeping when awake: %v", r.body)
	}

	e = newTestEnv(t, "")
	e.fakeOnline("fellow", fakeOpts{setup: func(c *client) { c.sleeping = true }})
	if r := e.do("GET", "/v1/accounts/fellow/fn/is_sleeping", ""); r.body["result"] != true {
		t.Fatalf("is_sleeping when asleep: %v", r.body)
	}
}

func TestWalkPathToRejectsBadCoordinates(t *testing.T) {
	e := newTestEnv(t, "")
	fa := e.fakeOnline("fellow", fakeOpts{})

	e.do("POST", "/v1/accounts/fellow/fn/walk_path_to", `{"x":-5,"z":640}`).want(t, 400, "bad_argument")
	e.do("POST", "/v1/accounts/fellow/fn/walk_path_to", `{"x":167}`).want(t, 400, "bad_argument")
	fa.noPacket(t, 100*time.Millisecond)
}

func TestCalculatePathEdges(t *testing.T) {
	e := newTestEnv(t, "")
	// Open ground (a map with no landscape loaded), with something in view, a
	// closed door say, blocking the target tile.
	e.fakeOnline("fellow", fakeOpts{setup: func(c *client) {
		c.x, c.z, c.world, c.localWorld = 220, 634, newWorld(), newLocalWorld()
		c.localWorld.setTileOr(223, 634, fullBlockC)
	}})
	fn := "/v1/accounts/fellow/fn/"

	// Counting what's in view, there's no path: the result is null.
	if r := e.do("GET", fn+"calculate_path_to?x=223&z=634&max_depth=8", ""); r.status != 200 || r.body["result"] != nil {
		t.Fatalf("blocked path: %d %v", r.status, r.body)
	}
	// skip_local leaves what's in view out, so the same target has a path.
	if r := e.do("GET", fn+"calculate_path_to?x=223&z=634&max_depth=8&skip_local=true", ""); r.status != 200 || r.body["result"] == nil {
		t.Fatalf("path with skip_local: %d %v", r.status, r.body)
	}
	// A path that starts where it ends would make plutonium loop forever.
	e.do("GET", fn+"calculate_path_to?x=220&z=634", "").want(t, 400, "bad_argument")
	e.do("GET", fn+"calculate_path?start_x=5&start_z=6&x=5&z=6", "").want(t, 400, "bad_argument")
	e.do("GET", fn+"calculate_path_to?x=223&z=634&depth=0", "").want(t, 400, "bad_argument")
	e.do("GET", fn+"calculate_path_to?x=223&z=634&max_depth=1024", "").want(t, 400, "bad_argument")
	e.do("GET", fn+"calculate_path_to?x=223&z=634&skip_local=maybe", "").want(t, 400, "bad_argument")
}

func TestFnRequestErrors(t *testing.T) {
	e := newTestEnv(t, "")
	fa := e.fakeOnline("fellow", fakeOpts{})

	tests := []struct {
		name, method, path, body string
		status                   int
		code                     string
	}{
		{"action called with GET", "GET", "/v1/accounts/fellow/fn/send_chat_message", "", 405, "wrong_method"},
		{"query called with POST", "POST", "/v1/accounts/fellow/fn/get_x", "{}", 405, "wrong_method"},
		{"unknown argument", "POST", "/v1/accounts/fellow/fn/send_chat_message", `{"message":"hi","colour":"red"}`, 400, "bad_argument"},
		{"missing argument", "POST", "/v1/accounts/fellow/fn/send_chat_message", `{}`, 400, "bad_argument"},
		{"wrong argument type", "POST", "/v1/accounts/fellow/fn/send_chat_message", `{"message":5}`, 400, "bad_argument"},
		{"malformed JSON", "POST", "/v1/accounts/fellow/fn/send_chat_message", `{`, 400, "bad_json"},
		{"query string argument a query doesn't take", "GET", "/v1/accounts/fellow/fn/get_x?radius=3", "", 400, "bad_argument"},
		{"unknown function", "GET", "/v1/accounts/fellow/fn/teleport_home", "", 404, "unknown_function"},
		{"unknown account", "GET", "/v1/accounts/nobody/fn/get_x", "", 404, "unknown_account"},
		{"username too long", "GET", "/v1/accounts/abcdefghijklm/fn/get_x", "", 400, "bad_username"},
		{"username with no letters", "GET", "/v1/accounts/___/fn/get_x", "", 400, "bad_username"},
		{"unknown route", "GET", "/v2/anything", "", 404, "not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.do(tt.method, tt.path, tt.body).want(t, tt.status, tt.code)
		})
	}

	// None of those reached the game.
	fa.noPacket(t, 50*time.Millisecond)

	r := e.do("GET", "/v1/accounts/fellow/fn/send_chat_message", "")
	if r.header.Get("Allow") != "POST" {
		t.Errorf("405 Allow = %q, want POST", r.header.Get("Allow"))
	}
}

func TestCallWhileNotOnlineIs409WithState(t *testing.T) {
	e := newTestEnv(t, "")
	ma := e.s.accounts.getOrCreate("fellow", "fellow")
	ma.setState(stateLoggingIn, "retrying: world_full")

	r := e.do("GET", "/v1/accounts/fellow/fn/get_x", "")
	r.want(t, 409, "account_offline")
	errBody := r.body["error"].(map[string]any)
	if errBody["state"] != "logging_in" || errBody["reason"] != "retrying: world_full" {
		t.Fatalf("got %v", errBody)
	}
}

func TestUnresponsiveLoopIs504(t *testing.T) {
	setVar(t, &callTimeout, 50*time.Millisecond)
	e := newTestEnv(t, "")
	e.fakeOnline("fellow", fakeOpts{noLoop: true})

	e.do("GET", "/v1/accounts/fellow/fn/get_x", "").want(t, 504, "loop_timeout")
}

func TestConnectionClosedDuringCallIs409(t *testing.T) {
	e := newTestEnv(t, "")
	fa := e.fakeOnline("fellow", fakeOpts{noLoop: true})
	time.AfterFunc(20*time.Millisecond, fa.c.close)

	e.do("GET", "/v1/accounts/fellow/fn/get_x", "").want(t, 409, "account_offline")
}

func TestPanicInCallIs500(t *testing.T) {
	register(&fnSpec{name: "test_panic", kind: fnQuery, call: func(c *client, _ fnArgs) (any, error) { panic("boom") }})
	t.Cleanup(func() { delete(functions, "test_panic") })
	e := newTestEnv(t, "")
	e.fakeOnline("fellow", fakeOpts{})

	e.do("GET", "/v1/accounts/fellow/fn/test_panic", "").want(t, 500, "adapter_panic")
	// The loop survives the panic and keeps serving.
	e.do("GET", "/v1/accounts/fellow/fn/get_x", "").want(t, 200, "")
}

// --- accounts and lifecycle ----------------------------------------------------

func TestLoginRequestErrors(t *testing.T) {
	e := newTestEnv(t, "")
	e.fakeOnline("fellow", fakeOpts{})

	e.do("POST", "/v1/accounts/someone/login", `{}`).want(t, 400, "bad_argument")
	e.do("POST", "/v1/accounts/someone/login", `{"password":"x","pin":1}`).want(t, 400, "bad_json")
	e.do("POST", "/v1/accounts/abcdefghijklm/login", `{"password":"x"}`).want(t, 400, "bad_username")

	r := e.do("POST", "/v1/accounts/Fellow/login", `{"password":"x"}`)
	r.want(t, 409, "already_running")
	if r.body["error"].(map[string]any)["state"] != "online" {
		t.Fatalf("got %v", r.body)
	}
}

func TestLogoutOfStoppedAccountIsIdempotent(t *testing.T) {
	e := newTestEnv(t, "")
	e.do("POST", "/v1/accounts/nobody/logout", "").want(t, 404, "unknown_account")

	e.s.accounts.getOrCreate("fellow", "fellow")
	r := e.do("POST", "/v1/accounts/fellow/logout", "")
	r.want(t, 202, "")
	if r.body["state"] != "stopped" {
		t.Fatalf("got %v", r.body)
	}
}

func TestLogoutSendsPacketOnceThenForcesClose(t *testing.T) {
	setVar(t, &logoutGrace, 50*time.Millisecond)
	e := newTestEnv(t, "")
	fa := e.fakeOnline("fellow", fakeOpts{})

	r := e.do("POST", "/v1/accounts/fellow/logout", "")
	r.want(t, 202, "")
	if r.body["state"] != "stopping" {
		t.Fatalf("got %v", r.body)
	}
	if op := fa.nextPacket(t); op != 102 {
		t.Fatalf("sent opcode %d, want 102 (logout)", op)
	}

	// Calls are refused once stopping.
	e.do("GET", "/v1/accounts/fellow/fn/get_x", "").want(t, 409, "account_offline")

	// A repeat logout is accepted but sends nothing.
	e.do("POST", "/v1/accounts/fellow/logout", "").want(t, 202, "")
	fa.noPacket(t, 20*time.Millisecond)

	// The fake server never drops the connection, so the grace close must.
	select {
	case <-fa.c.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("connection not closed after logout grace")
	}
}

func TestListAccountsIsSortedByKey(t *testing.T) {
	e := newTestEnv(t, "")
	e.s.accounts.getOrCreate("zed", "Zed")
	e.s.accounts.getOrCreate("amy", "Amy")

	r := e.do("GET", "/v1/accounts", "")
	r.want(t, 200, "")
	var keys []string
	for _, a := range r.body["accounts"].([]any) {
		keys = append(keys, a.(map[string]any)["key"].(string))
	}
	if !sort.StringsAreSorted(keys) || len(keys) != 2 {
		t.Fatalf("keys %v", keys)
	}
}

func TestRegisterRequestErrors(t *testing.T) {
	e := newTestEnv(t, "")
	// All rejected before contacting the game server.
	e.do("POST", "/v1/accounts", `{"password":"x"}`).want(t, 400, "bad_argument")
	e.do("POST", "/v1/accounts", `{"username":"fellow"}`).want(t, 400, "bad_argument")
	e.do("POST", "/v1/accounts", `{"username":"abcdefghijklm","password":"x"}`).want(t, 400, "bad_username")
	e.do("POST", "/v1/accounts", `{"username":"fellow","password":"x","age":3}`).want(t, 400, "bad_json")
}

func TestRegistrationErrorMapping(t *testing.T) {
	tests := []struct {
		code   byte
		status int
		apiErr string
	}{
		{2, 409, "username_taken"},
		{3, 409, "email_in_use"},
		{4, 403, "registration_disabled"},
		{5, 429, "registered_too_recently"},
		{6, 400, "bad_email"},
		{7, 400, "bad_username"},
		{8, 400, "bad_username"},
		{255, 502, "registration_failed"},
	}
	if registrationError(0, nil) != nil {
		t.Error("code 0 (created) should not be an error")
	}
	for _, tt := range tests {
		wantAPIError(t, registrationError(tt.code, nil), tt.status, tt.apiErr)
	}
	if got := registrationError(5, nil).header["Retry-After"]; got != "300" {
		t.Errorf("Retry-After = %q, want 300", got)
	}
}

func TestBearerToken(t *testing.T) {
	e := newTestEnv(t, "s3cret")
	e.do("GET", "/v1/accounts", "").want(t, 401, "unauthorized")
	e.do("GET", "/v1/accounts", "", "Authorization", "Bearer wrong").want(t, 401, "unauthorized")
	e.do("GET", "/v1/accounts", "", "Authorization", "Bearer s3cret").want(t, 200, "")
}
