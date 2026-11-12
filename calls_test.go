package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
)

func bodyArgs(t *testing.T, js string) fnArgs {
	t.Helper()
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatalf("bad test JSON %q: %s", js, err)
	}
	return fnArgs{body: m}
}

// wantAPIError checks err is an *apiError with the given status and code.
func wantAPIError(t *testing.T, err error, status int, code string) {
	t.Helper()
	var ae *apiError
	if !errors.As(err, &ae) {
		t.Fatalf("got error %v, want *apiError %d %s", err, status, code)
	}
	if ae.status != status || ae.code != code {
		t.Fatalf("got %d %s (%s), want %d %s", ae.status, ae.code, ae.message, status, code)
	}
}

func TestFnSpecValidate(t *testing.T) {
	spec := &fnSpec{
		name:   "f",
		params: []param{{name: "message", required: true}, {name: "radius"}},
	}
	tests := []struct {
		name string
		args fnArgs
		ok   bool
	}{
		{"required present", bodyArgs(t, `{"message":"hi"}`), true},
		{"optional present", bodyArgs(t, `{"message":"hi","radius":3}`), true},
		{"required missing", bodyArgs(t, `{"radius":3}`), false},
		{"unknown argument", bodyArgs(t, `{"message":"hi","colour":"red"}`), false},
		{"query required present", fnArgs{query: url.Values{"message": {"hi"}}}, true},
		{"query unknown argument", fnArgs{query: url.Values{"message": {"hi"}, "x": {"1"}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := spec.validate(tt.args)
			if tt.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tt.ok {
				wantAPIError(t, err, http.StatusBadRequest, "bad_argument")
			}
		})
	}
}

func TestFnArgsTypes(t *testing.T) {
	if s, err := bodyArgs(t, `{"m":"hi"}`).String("m"); err != nil || s != "hi" {
		t.Errorf("body String = %q, %v", s, err)
	}
	if _, err := bodyArgs(t, `{"m":5}`).String("m"); err == nil {
		t.Error("body String accepted a number")
	}
	if n, err := bodyArgs(t, `{"n":7}`).Int("n"); err != nil || n != 7 {
		t.Errorf("body Int = %d, %v", n, err)
	}
	if _, err := bodyArgs(t, `{"n":"7"}`).Int("n"); err == nil {
		t.Error("body Int accepted a string")
	}
	if n, err := (fnArgs{query: url.Values{"n": {"12"}}}).Int("n"); err != nil || n != 12 {
		t.Errorf("query Int = %d, %v", n, err)
	}
	_, err := (fnArgs{query: url.Values{"n": {"twelve"}}}).Int("n")
	wantAPIError(t, err, http.StatusBadRequest, "bad_argument")
}

func TestParseObjectRef(t *testing.T) {
	id, x, z, err := parseObjectRef("object", "object/1@170,640")
	if err != nil || id != 1 || x != 170 || z != 640 {
		t.Fatalf("got %d %d %d %v", id, x, z, err)
	}
	for _, ref := range []string{
		"", "object/1", "object/1@170", "npc/1@170,640", "object/a@170,640",
		"object/1@170,640,2", "object/01@170,640", "object/1@+170,640", " object/1@170,640",
	} {
		_, _, _, err := parseObjectRef("object", ref)
		wantAPIError(t, err, http.StatusBadRequest, "bad_argument")
	}
}

func TestParseWallObjectRef(t *testing.T) {
	id, x, z, dir, err := parseWallObjectRef("wall_object", "wall/2@170,641:1")
	if err != nil || id != 2 || x != 170 || z != 641 || dir != 1 {
		t.Fatalf("got %d %d %d %d %v", id, x, z, dir, err)
	}
	for _, ref := range []string{
		"", "wall/2", "wall/2@170,641", "object/2@170,641:1", "wall/a@170,641:1", "wall/2@170,641:x",
		"wall/02@170,641:1", "wall/2@170,641:+1", " wall/2@170,641:1", "wall/2@170,641:1:0",
	} {
		_, _, _, _, err := parseWallObjectRef("wall_object", ref)
		wantAPIError(t, err, http.StatusBadRequest, "bad_argument")
	}
}

func TestParseNPCRef(t *testing.T) {
	index, id, err := parseNPCRef("npc", "npc/412:id=65")
	if err != nil || index != 412 || id != 65 {
		t.Fatalf("got %d %d %v", index, id, err)
	}
	for _, ref := range []string{
		"", "npc/412", "npc/412:65", "npc/412:id=", "object/412:id=65", "npc/a:id=65",
		"npc/0412:id=65", "npc/412:id=+65", " npc/412:id=65", "npc/412:id=65:x",
	} {
		_, _, err := parseNPCRef("npc", ref)
		wantAPIError(t, err, http.StatusBadRequest, "bad_argument")
	}
}

func TestCoordinateArgument(t *testing.T) {
	if n, err := bodyArgs(t, `{"x":167}`).coordinate("x"); err != nil || n != 167 {
		t.Fatalf("got %d, %v", n, err)
	}
	for _, js := range []string{`{"x":-1}`, `{"x":40000}`, `{"x":1.5}`} {
		_, err := bodyArgs(t, js).coordinate("x")
		wantAPIError(t, err, http.StatusBadRequest, "bad_argument")
	}
}

func TestCallInvokeRecoversPanic(t *testing.T) {
	cl := &call{
		fn:    func(c *client) (any, error) { panic("boom") },
		reply: make(chan callResult, 1),
	}
	cl.invoke(&client{user: "test"})
	r := <-cl.reply
	wantAPIError(t, r.err, http.StatusInternalServerError, "adapter_panic")
}

func TestRegistryHasNoDuplicateNames(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("registering a duplicate name did not panic")
		}
	}()
	register(&fnSpec{name: "get_x"})
}
