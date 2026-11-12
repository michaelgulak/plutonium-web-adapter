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
	"strings"
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

// --- entity refs (ARCHITECTURE.md §7, "Entity references") ---------------------

func goneError(ref string) error {
	return &apiError{status: http.StatusGone, code: "entity_gone", message: ref + " is no longer there", extra: map[string]any{"ref": ref}}
}

// parseObjectRef reads "object/<id>@<x>,<z>". Only the canonical spelling is
// accepted, so a ref round-trips exactly.
func parseObjectRef(arg, ref string) (id, x, z int, err error) {
	bad := badArg(arg, "is not an object ref (object/<id>@<x>,<z>)")
	rest, ok := strings.CutPrefix(ref, "object/")
	if !ok {
		return 0, 0, 0, bad
	}
	idPart, at, ok := strings.Cut(rest, "@")
	if !ok {
		return 0, 0, 0, bad
	}
	xPart, zPart, ok := strings.Cut(at, ",")
	if !ok {
		return 0, 0, 0, bad
	}
	var errs [3]error
	id, errs[0] = strconv.Atoi(idPart)
	x, errs[1] = strconv.Atoi(xPart)
	z, errs[2] = strconv.Atoi(zPart)
	if errors.Join(errs[:]...) != nil || fmt.Sprintf("object/%d@%d,%d", id, x, z) != ref {
		return 0, 0, 0, bad
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

// parseNPCRef reads "npc/<index>:id=<id>", where index is the NPC's server
// index (a slot the server reuses) and id its type. Only the canonical
// spelling is accepted, so a ref round-trips exactly.
func parseNPCRef(arg, ref string) (index, id int, err error) {
	bad := badArg(arg, "is not an npc ref (npc/<index>:id=<id>)")
	rest, ok := strings.CutPrefix(ref, "npc/")
	if !ok {
		return 0, 0, bad
	}
	indexPart, idPart, ok := strings.Cut(rest, ":id=")
	if !ok {
		return 0, 0, bad
	}
	var errs [2]error
	index, errs[0] = strconv.Atoi(indexPart)
	id, errs[1] = strconv.Atoi(idPart)
	if errors.Join(errs[:]...) != nil || fmt.Sprintf("npc/%d:id=%d", index, id) != ref {
		return 0, 0, bad
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
// woodcutting needs (FELLOW-16), banking (FELLOW-18) and sleeping (FELLOW-14).
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
