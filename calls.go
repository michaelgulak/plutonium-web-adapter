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

// The first handful of functions, enough to prove the path end to end.
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
	)
}
