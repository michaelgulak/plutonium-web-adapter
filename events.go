package main

// Serve-mode event stream (FELLOW-4, SSE event stream).
//
// The game pushes things the brain must hear about (chat, damage, spawns).
// Each account keeps a ring buffer of its recent events, numbered per
// account for the life of the process, so numbering survives relogins. A
// subscriber is only a cursor into the ring: it reads at its own pace, the
// game loop never waits for it, and memory never grows. A reconnect sends
// Last-Event-ID and resumes from the ring; a reader whose next event has
// already been overwritten gets a resync event and should re-query state.
//
// Events are hints; state queries are the truth.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Ring size per account, set by -event-buffer. Logging in alone can emit a
// few hundred spawn events, so this is sized well above that.
var eventBufferSize = 4096

// How often an idle stream sends a heartbeat comment, so dead connections
// are noticed on both sides. A variable so tests can shorten it.
var sseHeartbeat = 15 * time.Second

// eventEpoch prefixes every event id, so a Last-Event-ID from an earlier
// process (whose numbering restarted at 1) is recognised instead of
// silently matching unrelated events.
var eventEpoch = strconv.FormatInt(time.Now().UnixNano(), 36)

// Game hooks streamed as events, named as plutonium's on_* hooks without
// the prefix. on_load is covered by the session event (state "online");
// on_init, on_bot_message and on_progress_report belong to the script
// engine, which serve mode doesn't run.
var gameEvents = []string{
	"chat_message",
	"death",
	"fatigue_update",
	"ground_item_despawned",
	"ground_item_spawned",
	"kill_signal",
	"npc_damaged",
	"npc_despawned",
	"npc_message",
	"npc_projectile",
	"npc_spawned",
	"object_despawned",
	"object_spawned",
	"option_menu",
	"player_damaged",
	"private_message",
	"server_message",
	"server_tick",
	"sleep_word_sent",
	"system_update",
	"trade_request",
	"wall_object_despawned",
	"wall_object_spawned",
}

// Adapter events, always delivered whatever the filter.
const (
	evSession = "session"
	evResync  = "resync"
	evTick    = "server_tick"
)

type event struct {
	id   uint64
	name string
	data []byte // JSON
}

type eventRing struct {
	mu   sync.Mutex
	buf  []event
	next uint64 // id the next event gets; ids start at 1
	tick uint64 // server ticks seen; kept out of the ring (see serverTick)
	wake chan struct{}

	readers atomic.Int32
}

func newEventRing(size int) *eventRing {
	if size < 1 {
		size = 1
	}
	return &eventRing{buf: make([]event, size), next: 1, wake: make(chan struct{})}
}

// wakeLocked tells every waiting reader something changed.
func (r *eventRing) wakeLocked() {
	close(r.wake)
	r.wake = make(chan struct{})
}

// publish appends an event. data is marshalled here, before the caller can
// mutate it, so payloads are value copies of the game state at that moment.
func (r *eventRing) publish(name string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"marshal_error": err.Error()})
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[(r.next-1)%uint64(len(r.buf))] = event{id: r.next, name: name, data: b}
	r.next++
	r.wakeLocked()
}

// serverTick counts a server tick. Ticks arrive every 640 ms and would
// flush everything else out of the ring, and only the latest one matters
// for pacing, so readers that opt in get the newest count, coalesced.
func (r *eventRing) serverTick() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tick++
	r.wakeLocked()
}

// last returns the id of the newest event, or 0 if there is none.
func (r *eventRing) last() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.next - 1
}

// ringRead is what a reader at a cursor sees.
type ringRead struct {
	events []event
	lost   bool   // events after the cursor were overwritten
	cursor uint64 // where reading resumed: lost ? oldest-1 : the given cursor
	tick   uint64
	wake   <-chan struct{}
}

// since returns the events after cursor `after`. If some were overwritten,
// or the cursor is ahead of the ring (it can't have come from this ring),
// it reports lost and resumes from the oldest event still held.
func (r *eventRing) since(after uint64) ringRead {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := ringRead{cursor: after, tick: r.tick, wake: r.wake}

	oldest := uint64(1)
	if size := uint64(len(r.buf)); r.next-1 > size {
		oldest = r.next - size
	}
	if after+1 < oldest || after >= r.next {
		out.lost = true
		after = oldest - 1
		out.cursor = after
	}
	for id := after + 1; id < r.next; id++ {
		out.events = append(out.events, r.buf[(id-1)%uint64(len(r.buf))])
	}
	return out
}

// --- emitting from the client ------------------------------------------------

// The emit helpers are no-ops outside serve mode (c.events is nil), and
// build their payloads only when someone will see them.

func (c *client) emit(name string, data any) {
	if c.events != nil {
		c.events.publish(name, data)
	}
}

func (c *client) emitServerTick() {
	if c.events != nil {
		c.events.serverTick()
	}
}

func (c *client) emitNPC(name string, n *npc) {
	if c.events != nil {
		c.events.publish(name, map[string]any{"npc": npcViewOf(n)})
	}
}

func (c *client) emitObject(name string, o *object) {
	if c.events != nil {
		c.events.publish(name, map[string]any{"object": objectViewOf(o)})
	}
}

func (c *client) emitWallObject(name string, o *wallObject) {
	if c.events != nil {
		c.events.publish(name, map[string]any{"wall_object": wallObjectViewOf(o)})
	}
}

func (c *client) emitGroundItem(name string, g *groundItem) {
	if c.events != nil {
		c.events.publish(name, map[string]any{"ground_item": groundItemViewOf(g)})
	}
}

// emitMessage covers chat_message and private_message: text from a named player.
func (c *client) emitMessage(name, message, from string) {
	if c.events != nil {
		c.events.publish(name, map[string]any{"message": message, "from": nameViewOf(from)})
	}
}

// --- payload views and entity refs ---------------------------------------------
//
// Entities carry a deterministic natural-key ref (ARCHITECTURE §7, entity
// references), the same strings FELLOW-11 (adapter call surface) will
// accept back: the adapter resolves a ref against live state, so there is
// no handle registry to leak or go stale.

type npcView struct {
	Ref   string `json:"ref"`
	ID    int    `json:"id"`
	X     int    `json:"x"`
	Z     int    `json:"z"`
	HP    int    `json:"hp"`
	MaxHP int    `json:"max_hp"`
}

type playerView struct {
	Ref         string `json:"ref,omitempty"`
	Username    string `json:"username"`
	X           int    `json:"x"`
	Z           int    `json:"z"`
	HP          int    `json:"hp"`
	MaxHP       int    `json:"max_hp"`
	CombatLevel int    `json:"combat_level"`
}

type nameView struct {
	Ref      string `json:"ref,omitempty"`
	Username string `json:"username"`
}

type objectView struct {
	Ref string `json:"ref"`
	ID  int    `json:"id"`
	X   int    `json:"x"`
	Z   int    `json:"z"`
	Dir int    `json:"dir"`
}

type groundItemView struct {
	Ref string `json:"ref"`
	ID  int    `json:"id"`
	X   int    `json:"x"`
	Z   int    `json:"z"`
}

func npcRef(n *npc) string { return fmt.Sprintf("npc/%d:id=%d", n.serverIndex, n.id) }

// playerRef is "player/<key>", or "" while the player's name is unknown
// (its appearance update hasn't arrived yet).
func playerRef(username string) string {
	if key := accountKey(username); key != "" {
		return "player/" + key
	}
	return ""
}

func objectRef(o *object) string { return fmt.Sprintf("object/%d@%d,%d", o.id, o.x, o.z) }

func wallObjectRef(o *wallObject) string {
	return fmt.Sprintf("wall/%d@%d,%d:%d", o.id, o.x, o.z, o.dir)
}

func groundItemRef(g *groundItem) string { return fmt.Sprintf("ground/%d@%d,%d", g.id, g.x, g.z) }

func npcViewOf(n *npc) npcView {
	return npcView{Ref: npcRef(n), ID: n.id, X: n.x, Z: n.z, HP: n.currentHP, MaxHP: n.maxHP}
}

func playerViewOf(p *player) playerView {
	return playerView{
		Ref:         playerRef(p.username),
		Username:    p.username,
		X:           p.x,
		Z:           p.z,
		HP:          p.currentHP,
		MaxHP:       p.maxHP,
		CombatLevel: p.combatLevel,
	}
}

func nameViewOf(username string) nameView {
	return nameView{Ref: playerRef(username), Username: username}
}

func objectViewOf(o *object) objectView {
	return objectView{Ref: objectRef(o), ID: o.id, X: o.x, Z: o.z, Dir: o.dir}
}

func wallObjectViewOf(o *wallObject) objectView {
	return objectView{Ref: wallObjectRef(o), ID: o.id, X: o.x, Z: o.z, Dir: o.dir}
}

func groundItemViewOf(g *groundItem) groundItemView {
	return groundItemView{Ref: groundItemRef(g), ID: g.id, X: g.x, Z: g.z}
}

// --- the SSE endpoint ------------------------------------------------------------

func eventID(n uint64) string { return eventEpoch + "-" + strconv.FormatUint(n, 10) }

// parseLastEventID turns a Last-Event-ID header into a ring cursor.
//
//   - absent: start at the live tail (only events from now on)
//   - "0": start from the oldest event the ring still holds, which is how
//     the brain sees everything since the login it just requested
//   - "<epoch>-<n>": resume after event n; an id from an earlier process
//     resumes from the oldest event, behind a resync
func parseLastEventID(header string, r *eventRing) (cursor uint64, resync bool, err error) {
	switch header {
	case "":
		return r.last(), false, nil
	case "0":
		return 0, false, nil
	}
	epoch, num, ok := strings.Cut(header, "-")
	n, perr := strconv.ParseUint(num, 10, 64)
	if !ok || perr != nil {
		return 0, false, &apiError{status: http.StatusBadRequest, code: "bad_argument", message: "Last-Event-ID must be 0 or an id from this stream"}
	}
	if epoch != eventEpoch {
		return 0, true, nil
	}
	return n, false, nil
}

// parseEventFilter reads ?events=a,b,c. With no filter every event except
// server_tick is sent; with one, only the named events are. session and
// resync are always sent: a reader can't stay correct without them.
func parseEventFilter(raw string) (map[string]bool, error) {
	if raw == "" {
		want := map[string]bool{}
		for _, name := range gameEvents {
			want[name] = name != evTick
		}
		return want, nil
	}
	known := map[string]bool{evSession: true, evResync: true}
	for _, name := range gameEvents {
		known[name] = true
	}
	want := map[string]bool{}
	var unknown []string
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if !known[name] {
			unknown = append(unknown, name)
			continue
		}
		want[name] = true
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, &apiError{
			status:  http.StatusBadRequest,
			code:    "bad_argument",
			message: "unknown event names: " + strings.Join(unknown, ", "),
			extra:   map[string]any{"events": gameEvents},
		}
	}
	return want, nil
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	ma, err := s.accountFromPath(r)
	if err != nil {
		writeError(w, err)
		return
	}
	want, err := parseEventFilter(r.URL.Query().Get("events"))
	if err != nil {
		writeError(w, err)
		return
	}
	ring := ma.events
	cursor, resync, err := parseLastEventID(r.Header.Get("Last-Event-ID"), ring)
	if err != nil {
		writeError(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, &apiError{status: http.StatusInternalServerError, code: "internal", message: "streaming unsupported"})
		return
	}

	ring.readers.Add(1)
	defer ring.readers.Add(-1)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()

	sentID := cursor                    // id of the last event this stream told the reader about
	lastTick := ring.since(cursor).tick // ticks before connecting are stale

	for {
		read := ring.since(cursor)
		var out strings.Builder

		if read.lost || resync {
			reason := "events_lost"
			if resync {
				reason = "new_process"
			}
			writeSSE(&out, evResync, read.cursor, []byte(`{"reason":"`+reason+`"}`))
			sentID = read.cursor
			resync = false
		}
		cursor = read.cursor
		for _, ev := range read.events {
			cursor = ev.id
			if ev.name == evSession || want[ev.name] {
				writeSSE(&out, ev.name, ev.id, ev.data)
				sentID = ev.id
			}
		}
		if want[evTick] && read.tick != lastTick {
			// No id: ticks aren't in the ring, so they mustn't move the cursor.
			fmt.Fprintf(&out, "event: %s\ndata: {\"tick\":%d}\n\n", evTick, read.tick)
			lastTick = read.tick
		}

		if out.Len() > 0 {
			if _, err := w.Write([]byte(out.String())); err != nil {
				return
			}
			flusher.Flush()
		}

		select {
		case <-read.wake:
		case <-heartbeat.C:
			// A filtered reader can go a long time without a delivered
			// event. An id-only block moves its Last-Event-ID past the
			// events it skipped, so a reconnect doesn't reread them (or
			// resync because they've since been overwritten).
			msg := ": heartbeat\n\n"
			if cursor != sentID {
				msg = ": heartbeat\nid: " + eventID(cursor) + "\n\n"
				sentID = cursor
			}
			if _, err := w.Write([]byte(msg)); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		case <-s.closing:
			return
		}
	}
}

func writeSSE(out *strings.Builder, name string, id uint64, data []byte) {
	fmt.Fprintf(out, "id: %s\nevent: %s\ndata: %s\n\n", eventID(id), name, data)
}
