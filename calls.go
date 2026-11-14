package main

// The per-account call channel and the function registry.
//
// A *client's state is only ever touched by its own eventLoop goroutine.
// HTTP handlers never read or write it directly: they wrap the work in a
// closure, send it on c.calls, and the event loop runs it between packets
// and sends the result back (the same idea as a main-thread dispatcher).

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"time"
)

// How long a call waits for the account's event loop. A variable so tests
// can shorten it.
var callTimeout = 10 * time.Second

type callResult struct {
	value any
	err   error
}

type call struct {
	fn    func(c *client) (any, error)
	reply chan callResult // buffered (1) so the event loop never blocks on it
}

// invoke runs the closure on the event loop goroutine. A panic is an adapter
// bug, so it is recovered and reported rather than taking the process down.
func (cl *call) invoke(c *client) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[%s] Panic in call: %v\n%s", c.user, r, debug.Stack())
			cl.reply <- callResult{err: &apiError{
				status:  http.StatusInternalServerError,
				code:    "adapter_panic",
				message: fmt.Sprint(r),
			}}
		}
	}()
	v, err := cl.fn(c)
	cl.reply <- callResult{value: v, err: err}
}

var errAccountOffline = errors.New("account offline")

// callClient runs fn on c's event loop and waits for the result.
func (s *server) callClient(c *client, fn func(c *client) (any, error)) (any, error) {
	cl := &call{fn: fn, reply: make(chan callResult, 1)}
	timer := time.NewTimer(callTimeout)
	defer timer.Stop()

	select {
	case c.calls <- cl:
	case <-c.closed:
		return nil, errAccountOffline
	case <-timer.C:
		return nil, &apiError{status: http.StatusGatewayTimeout, code: "loop_timeout", message: "account event loop did not accept the call in time"}
	}

	select {
	case r := <-cl.reply:
		return r.value, r.err
	case <-c.closed:
		return nil, errAccountOffline
	case <-timer.C:
		return nil, &apiError{status: http.StatusGatewayTimeout, code: "loop_timeout", message: "account event loop did not answer in time"}
	}
}

// fnArgs holds a call's arguments: a JSON object body for actions (POST),
// or the query string for queries (GET).
type fnArgs struct {
	body  map[string]json.RawMessage
	query url.Values
}

func (a fnArgs) has(name string) bool {
	if a.body != nil {
		_, ok := a.body[name]
		return ok
	}
	_, ok := a.query[name]
	return ok
}

func (a fnArgs) names() []string {
	var out []string
	if a.body != nil {
		for k := range a.body {
			out = append(out, k)
		}
		return out
	}
	for k := range a.query {
		out = append(out, k)
	}
	return out
}

func badArg(name, msg string) error {
	return &apiError{status: http.StatusBadRequest, code: "bad_argument", message: fmt.Sprintf("%s: %s", name, msg), extra: map[string]any{"argument": name}}
}

func (a fnArgs) String(name string) (string, error) {
	if a.body != nil {
		var s string
		if err := json.Unmarshal(a.body[name], &s); err != nil {
			return "", badArg(name, "must be a string")
		}
		return s, nil
	}
	return a.query.Get(name), nil
}

func (a fnArgs) Int(name string) (int, error) {
	if a.body != nil {
		var n int
		if err := json.Unmarshal(a.body[name], &n); err != nil {
			return 0, badArg(name, "must be an integer")
		}
		return n, nil
	}
	n, err := strconv.Atoi(a.query.Get(name))
	if err != nil {
		return 0, badArg(name, "must be an integer")
	}
	return n, nil
}

func (a fnArgs) Bool(name string) (bool, error) {
	if a.body != nil {
		var b bool
		if err := json.Unmarshal(a.body[name], &b); err != nil {
			return false, badArg(name, "must be true or false")
		}
		return b, nil
	}
	b, err := strconv.ParseBool(a.query.Get(name))
	if err != nil {
		return false, badArg(name, "must be true or false")
	}
	return b, nil
}

type fnKind int

const (
	fnQuery  fnKind = iota // reads state only; served as GET
	fnAction               // does something in game; served as POST
)

type param struct {
	name     string
	required bool
}

// fnSpec is one row of the registry: a plutonium script function, served
// under its verbatim name.
type fnSpec struct {
	name   string
	kind   fnKind
	params []param
	// call runs on the account's event loop goroutine.
	call func(c *client, args fnArgs) (any, error)
}

// validate rejects unknown and missing arguments, so a typo in the brain
// fails loudly instead of being ignored.
func (f *fnSpec) validate(args fnArgs) error {
	known := map[string]bool{}
	for _, p := range f.params {
		known[p.name] = true
		if p.required && !args.has(p.name) {
			return badArg(p.name, "is required")
		}
	}
	for _, n := range args.names() {
		if !known[n] {
			return badArg(n, "is not a parameter of "+f.name)
		}
	}
	return nil
}

var functions = map[string]*fnSpec{}

func register(specs ...*fnSpec) {
	for _, f := range specs {
		if _, dup := functions[f.name]; dup {
			panic("duplicate function registration: " + f.name)
		}
		functions[f.name] = f
	}
}

// optionalInt reads an integer argument that may be left out, answering def
// when it is; given, it must lie between lo and hi.
func (a fnArgs) optionalInt(name string, lo, hi, def int) (int, error) {
	if !a.has(name) {
		return def, nil
	}
	n, err := a.Int(name)
	if err != nil {
		return 0, err
	}
	if n < lo || n > hi {
		return 0, badArg(name, fmt.Sprintf("must be between %d and %d", lo, hi))
	}
	return n, nil
}

// coordinate reads an integer argument that must fit a world coordinate.
func (a fnArgs) coordinate(name string) (int16, error) {
	n, err := a.Int(name)
	if err != nil {
		return 0, err
	}
	if n < 0 || n > math.MaxInt16 {
		return 0, badArg(name, "is not a world coordinate")
	}
	return int16(n), nil
}

// --- paths (SERVE.md, "Calls") -----------------------------------------------------

// pathParams are calculate_path's and calculate_path_to's optional arguments,
// with plutonium's own defaults (script.go:1050-1052): depth found by doubling,
// local objects counted (API.md says skip_local defaults to true; the code
// says false, and the code is what runs), max_depth 512.
var pathParams = []param{{name: "depth"}, {name: "skip_local"}, {name: "max_depth"}}

const (
	// depthLimit is walk_path_depth_to's documented limit for depth (API.md).
	depthLimit = 800
	// maxPathDepth is API.md's largest max_depth (its values are the powers of
	// two up to 512), and plutonium's default. Each doubling past a failed
	// depth builds a board of (2d+1)² nodes on the event loop.
	maxPathDepth = 512
)

// pathView is a plutonium path object's read-only side, length() and coords(),
// as a value: the object itself walks a cursor along the path and lives only
// in this process, so it has no ref to send.
type pathView struct {
	Length int         `json:"length"`
	Coords []tileCoord `json:"coords"`
}

type tileCoord struct {
	X int `json:"x"`
	Z int `json:"z"`
}

// calculatePath runs plutonium's generateLongPath from (sx, sz) to (x, z) with
// the optional arguments in args, answering a pathView, or nil for no path.
func calculatePath(c *client, args fnArgs, sx, sz, x, z int16) (any, error) {
	depth, err := args.optionalInt("depth", 1, depthLimit, -1)
	if err != nil {
		return nil, err
	}
	maxDepth, err := args.optionalInt("max_depth", 1, maxPathDepth, maxPathDepth)
	if err != nil {
		return nil, err
	}
	skipLocal := false
	if args.has("skip_local") {
		if skipLocal, err = args.Bool("skip_local"); err != nil {
			return nil, err
		}
	}
	// generateLongPath sizes its first board from the distance, so a zero
	// distance gives depth 0, which doubles to 0 forever (accessors.go:584-597).
	if sx == x && sz == z {
		return nil, badArg("x", "the path would start where it ends")
	}

	_, coords := c.generateLongPath(sx, sz, x, z, int16(depth), skipLocal, int16(maxDepth))
	if coords == nil {
		return nil, nil
	}
	view := pathView{Length: len(coords), Coords: make([]tileCoord, len(coords))}
	for i, pt := range coords {
		view.Coords[i] = tileCoord{int(pt.x), int(pt.y)}
	}
	return view, nil
}

// --- entity refs (SERVE.md, "Entity references") -------------------------------

func goneError(ref string) error {
	return &apiError{status: http.StatusGone, code: "entity_gone", message: ref + " is no longer there", extra: map[string]any{"ref": ref}}
}

// scanRef reads ref by format, whose only verbs are %d, into fields. Only the
// canonical spelling is accepted: the ref must print back exactly as given,
// which rules out the signs, spaces, leading zeros and trailing text that
// Sscanf alone would let through, so a ref round-trips exactly.
func scanRef(ref, format string, fields ...*int) bool {
	scan := make([]any, len(fields))
	for i, f := range fields {
		scan[i] = f
	}
	if _, err := fmt.Sscanf(ref, format, scan...); err != nil {
		return false
	}
	values := make([]any, len(fields))
	for i, f := range fields {
		values[i] = *f
	}
	return fmt.Sprintf(format, values...) == ref
}

// parseObjectRef reads "object/<id>@<x>,<z>".
func parseObjectRef(arg, ref string) (id, x, z int, err error) {
	if !scanRef(ref, "object/%d@%d,%d", &id, &x, &z) {
		return 0, 0, 0, badArg(arg, "is not an object ref (object/<id>@<x>,<z>)")
	}
	return id, x, z, nil
}

// objectArg resolves an object ref against what the client sees right now.
// A different object at those coordinates (a tree that became a stump) is
// gone too, which is why the ref carries the id.
func objectArg(c *client, args fnArgs, name string) (*object, error) {
	ref, err := args.String(name)
	if err != nil {
		return nil, err
	}
	id, x, z, err := parseObjectRef(name, ref)
	if err != nil {
		return nil, err
	}
	o := c.getObjectFromCoords(x, z)
	if o == nil || o.id != id {
		return nil, goneError(ref)
	}
	return o, nil
}

// parseWallObjectRef reads "wall/<id>@<x>,<z>:<dir>". A tile can hold one wall
// object per edge, so the direction is part of the key.
func parseWallObjectRef(arg, ref string) (id, x, z, dir int, err error) {
	if !scanRef(ref, "wall/%d@%d,%d:%d", &id, &x, &z, &dir) {
		return 0, 0, 0, 0, badArg(arg, "is not a wall object ref (wall/<id>@<x>,<z>:<dir>)")
	}
	return id, x, z, dir, nil
}

// wallObjectArg resolves a wall object ref against what the client sees right
// now. A door that has become a doorframe is gone, which is why the ref
// carries the id.
func wallObjectArg(c *client, args fnArgs, name string) (*wallObject, error) {
	ref, err := args.String(name)
	if err != nil {
		return nil, err
	}
	id, x, z, dir, err := parseWallObjectRef(name, ref)
	if err != nil {
		return nil, err
	}
	for i := 0; i < c.wallObjectCount; i++ {
		if w := c.wallObjects[i]; w.x == x && w.z == z && w.dir == dir {
			if w.id != id {
				break
			}
			return w, nil
		}
	}
	return nil, goneError(ref)
}

// parseNPCRef reads "npc/<index>:id=<id>", where index is the NPC's server
// index (a slot the server reuses) and id its type.
func parseNPCRef(arg, ref string) (index, id int, err error) {
	if !scanRef(ref, "npc/%d:id=%d", &index, &id) {
		return 0, 0, badArg(arg, "is not an npc ref (npc/<index>:id=<id>)")
	}
	return index, id, nil
}

// npcArg resolves an npc ref against the NPCs in view right now. The server
// reuses an index once its NPC leaves, so an NPC of another type at that
// index is gone too, which is why the ref carries the type id.
func npcArg(c *client, args fnArgs, name string) (*npc, error) {
	ref, err := args.String(name)
	if err != nil {
		return nil, err
	}
	index, id, err := parseNPCRef(name, ref)
	if err != nil {
		return nil, err
	}
	for i := 0; i < c.npcCount; i++ {
		if n := c.npcs[i]; n.serverIndex == index {
			if n.id != id {
				break
			}
			return n, nil
		}
	}
	return nil, goneError(ref)
}

// inventoryItemView is an inventory item as functions return it. The ref is
// the slot, which shifts when an earlier item leaves.
type inventoryItemView struct {
	Ref      string `json:"ref"`
	ID       int    `json:"id"`
	Amount   int    `json:"amount"`
	Equipped bool   `json:"equipped"`
	Slot     int    `json:"slot"`
}

func inventoryItemViewOf(it *item) inventoryItemView {
	return inventoryItemView{Ref: fmt.Sprintf("inv/%d", it.slot), ID: it.id, Amount: it.amount, Equipped: it.equipped, Slot: it.slot}
}

// The first handful of functions, enough to prove the path end to end, what
// woodcutting needs (FELLOW-16), banking (FELLOW-18), sleeping (FELLOW-14) and
// area transitions (FELLOW-25).
// The full surface is FELLOW-11.
func init() {
	register(
		&fnSpec{
			name: "get_x",
			kind: fnQuery,
			call: func(c *client, _ fnArgs) (any, error) { return c.x, nil },
		},
		&fnSpec{
			name: "get_z",
			kind: fnQuery,
			call: func(c *client, _ fnArgs) (any, error) { return c.z, nil },
		},
		&fnSpec{
			name: "get_fatigue",
			kind: fnQuery,
			call: func(c *client, _ fnArgs) (any, error) { return c.fatigue, nil },
		},
		&fnSpec{
			name:   "send_chat_message",
			kind:   fnAction,
			params: []param{{name: "message", required: true}},
			call: func(c *client, args fnArgs) (any, error) {
				msg, err := args.String("message")
				if err != nil {
					return nil, err
				}
				c.sendChatMessage(msg)
				return nil, nil
			},
		},

		// Walking, as plutonium's walk_path_to: find a path, then send one walk
		// packet up to ten waypoints along it. Call it again to keep going.
		// false means no path was found.
		&fnSpec{
			name:   "walk_path_to",
			kind:   fnAction,
			params: []param{{name: "x", required: true}, {name: "z", required: true}},
			call: func(c *client, args fnArgs) (any, error) {
				x, err := args.coordinate("x")
				if err != nil {
					return nil, err
				}
				z, err := args.coordinate("z")
				if err != nil {
					return nil, err
				}
				pt := c.generateLongPathPoint(x, z, -1)
				if pt == nil {
					return false, nil
				}
				c.walkTo(int(pt.x), int(pt.y), int(pt.x), int(pt.y), false, false)
				return true, nil
			},
		},

		// Paths, as plutonium's calculate_path_to and calculate_path: the
		// tiles of a path, without walking it. With skip_local, only the
		// landscape and fixed scenery count, so closed doors and gates are
		// passable. null means no path within max_depth.
		&fnSpec{
			name:   "calculate_path_to",
			kind:   fnQuery,
			params: append([]param{{name: "x", required: true}, {name: "z", required: true}}, pathParams...),
			call: func(c *client, args fnArgs) (any, error) {
				x, err := args.coordinate("x")
				if err != nil {
					return nil, err
				}
				z, err := args.coordinate("z")
				if err != nil {
					return nil, err
				}
				return calculatePath(c, args, int16(c.x), int16(c.z), x, z)
			},
		},
		&fnSpec{
			name: "calculate_path",
			kind: fnQuery,
			params: append([]param{
				{name: "start_x", required: true}, {name: "start_z", required: true},
				{name: "x", required: true}, {name: "z", required: true},
			}, pathParams...),
			call: func(c *client, args fnArgs) (any, error) {
				var coords [4]int16
				for i, name := range []string{"start_x", "start_z", "x", "z"} {
					n, err := args.coordinate(name)
					if err != nil {
						return nil, err
					}
					coords[i] = n
				}
				return calculatePath(c, args, coords[0], coords[1], coords[2], coords[3])
			},
		},

		// Objects.
		&fnSpec{
			name: "get_objects",
			kind: fnQuery,
			call: func(c *client, _ fnArgs) (any, error) {
				out := make([]objectView, c.objectCount)
				for i := range out {
					out[i] = objectViewOf(c.objects[i])
				}
				return out, nil
			},
		},
		&fnSpec{
			name:   "at_object",
			kind:   fnAction,
			params: []param{{name: "object", required: true}},
			call: func(c *client, args fnArgs) (any, error) {
				o, err := objectArg(c, args, "object")
				if err != nil {
					return nil, err
				}
				c.atObject(o)
				return nil, nil
			},
		},
		// The object's second command, e.g. a watch tower's "climb down".
		&fnSpec{
			name:   "at_object2",
			kind:   fnAction,
			params: []param{{name: "object", required: true}},
			call: func(c *client, args fnArgs) (any, error) {
				o, err := objectArg(c, args, "object")
				if err != nil {
					return nil, err
				}
				c.atObject2(o)
				return nil, nil
			},
		},

		// Wall objects: doors, doorframes, walls (area transitions, FELLOW-25).
		&fnSpec{
			name: "get_wall_objects",
			kind: fnQuery,
			call: func(c *client, _ fnArgs) (any, error) {
				out := make([]objectView, c.wallObjectCount)
				for i := range out {
					out[i] = wallObjectViewOf(c.wallObjects[i])
				}
				return out, nil
			},
		},
		&fnSpec{
			name:   "at_wall_object",
			kind:   fnAction,
			params: []param{{name: "wall_object", required: true}},
			call: func(c *client, args fnArgs) (any, error) {
				w, err := wallObjectArg(c, args, "wall_object")
				if err != nil {
					return nil, err
				}
				c.atWallObject(w)
				return nil, nil
			},
		},

		// Inventory.
		&fnSpec{
			name: "get_inventory_items",
			kind: fnQuery,
			call: func(c *client, _ fnArgs) (any, error) {
				out := make([]inventoryItemView, c.inventoryCount)
				for i := range out {
					out[i] = inventoryItemViewOf(c.inventory[i])
				}
				return out, nil
			},
		},
		&fnSpec{
			name: "get_empty_slots",
			kind: fnQuery,
			call: func(c *client, _ fnArgs) (any, error) { return 30 - c.inventoryCount, nil },
		},

		// NPCs (banking talks to a banker, FELLOW-18).
		&fnSpec{
			name: "get_npcs",
			kind: fnQuery,
			call: func(c *client, _ fnArgs) (any, error) {
				out := make([]npcView, c.npcCount)
				for i := range out {
					out[i] = npcViewOf(c.npcs[i])
				}
				return out, nil
			},
		},
		&fnSpec{
			name:   "talk_to_npc",
			kind:   fnAction,
			params: []param{{name: "npc", required: true}},
			call: func(c *client, args fnArgs) (any, error) {
				n, err := npcArg(c, args, "npc")
				if err != nil {
					return nil, err
				}
				c.talkToNPC(n)
				return nil, nil
			},
		},

		// The quest menu: the options a dialogue offers.
		&fnSpec{
			name: "is_option_menu",
			kind: fnQuery,
			call: func(c *client, _ fnArgs) (any, error) { return c.optionMenuVisible, nil },
		},
		&fnSpec{
			name: "get_option_menu",
			kind: fnQuery,
			call: func(c *client, _ fnArgs) (any, error) {
				return append([]string{}, c.optionMenu[:c.optionMenuCount]...), nil
			},
		},
		&fnSpec{
			name:   "answer",
			kind:   fnAction,
			params: []param{{name: "index", required: true}},
			call: func(c *client, args fnArgs) (any, error) {
				index, err := args.Int("index")
				if err != nil {
					return nil, err
				}
				// Against the options the last menu offered, not the array that holds them.
				if index < 0 || index >= c.optionMenuCount {
					return nil, badArg("index", fmt.Sprintf("must be 0 to %d, the last menu's options", c.optionMenuCount-1))
				}
				c.answer(index)
				return nil, nil
			},
		},

		// The bank.
		&fnSpec{
			name: "is_bank_open",
			kind: fnQuery,
			call: func(c *client, _ fnArgs) (any, error) { return c.banking, nil },
		},
		&fnSpec{
			name:   "deposit",
			kind:   fnAction,
			params: []param{{name: "id", required: true}, {name: "amount", required: true}},
			call: func(c *client, args fnArgs) (any, error) {
				id, err := args.Int("id")
				if err != nil {
					return nil, err
				}
				amount, err := args.Int("amount")
				if err != nil {
					return nil, err
				}
				if id < 0 || id > math.MaxUint16 {
					return nil, badArg("id", "is not an item id")
				}
				if amount <= 0 {
					return nil, badArg("amount", "must be positive")
				}
				c.deposit(id, amount)
				return nil, nil
			},
		},
		&fnSpec{
			name: "close_bank",
			kind: fnAction,
			call: func(c *client, _ fnArgs) (any, error) {
				c.closeBank()
				return nil, nil
			},
		},

		// Sleeping (FELLOW-14). The server asks the client for the sleep word
		// once the sleep screen shows; with no script running, plutonium
		// answers it by itself when the fatigue bar reaches zero.
		&fnSpec{
			name: "is_sleeping",
			kind: fnQuery,
			call: func(c *client, _ fnArgs) (any, error) { return c.sleeping, nil },
		},
		// false means no sleeping bag was carried and nothing was sent, as
		// walk_path_to's false means no path (plutonium returns nothing).
		&fnSpec{
			name: "use_sleeping_bag",
			kind: fnAction,
			call: func(c *client, _ fnArgs) (any, error) { return c.useSleepingBag(), nil },
		},
	)
}
