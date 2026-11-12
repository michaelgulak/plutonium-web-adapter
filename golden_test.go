package main

// Golden files (FELLOW-12, test framework, decision T4c): the exact JSON
// the adapter sends, saved under testdata/ and compared byte for byte.
// When a shape changes on purpose, rewrite them and review the diff:
//
//	go test -run Golden -update ./...
//
// The files are meant to double as example payloads for the Kotlin side
// (FELLOW-5, wire primitives; FELLOW-7, stub adapter), so they stay plain:
// one scenario per file, indented, named after the event or function.

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files under testdata/")

// golden compares JSON with testdata/<name>, or rewrites it under -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Indent(&buf, got, "", "  "); err != nil {
		t.Fatalf("%s: not JSON: %s", name, got)
	}
	buf.WriteByte('\n')
	path := filepath.Join("testdata", name)

	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no golden file %s (run: go test -run Golden -update ./...)", path)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("%s changed; if intended, run: go test -run Golden -update ./...\n--- got\n%s--- want\n%s", path, buf.Bytes(), want)
	}
}

// --- event payloads -------------------------------------------------------------

// goldenClient is a client with an event ring and no connection, holding
// one known NPC (server index 412) and one known player (server index 7).
func goldenClient() *client {
	c := &client{events: newEventRing(64), account: &account{}}
	c.init()
	c.npcsServer[412] = &npc{serverIndex: 412, id: 65, x: 130, z: 512, currentHP: 20, maxHP: 20}
	c.playersServer[7] = &player{serverIndex: 7, username: "Some Guy", x: 131, z: 512, currentHP: 10, maxHP: 10, combatLevel: 3}
	return c
}

// feed runs one server packet through the client's real packet handler.
func feed(c *client, opcode byte, payload ...[]byte) {
	buf := []byte{opcode}
	for _, p := range payload {
		buf = append(buf, p...)
	}
	p := &RSCPacket{buf: buf}
	op := p.readByteAsInt()
	c.handlePacket(p, op, len(p.buf)-1)
}

// eventScenarios produce each streamed event once. Where the setup allows,
// the event comes from a real server packet; spawn events need world data
// the tests don't load, so they go through the same emit helpers the
// packet handlers call.
var eventScenarios = map[string]func(c *client){
	"chat_message": func(c *client) {
		feed(c, 131, u32(0), u8(4), u8(1), str("hello bot"), str("Some Guy"), str("Some Guy"))
	},
	"private_message": func(c *client) {
		feed(c, 131, u32(0), u8(1), u8(1), str("psst"), str("Some Guy"), str("Some Guy"))
	},
	"server_message":  func(c *client) { feed(c, 131, u32(0), u8(0), u8(0), str("Welcome to RuneScape!")) },
	"trade_request":   func(c *client) { feed(c, 131, u32(0), u8(6), u8(1), str(""), str("Some Guy"), str("Some Guy")) },
	"fatigue_update":  func(c *client) { feed(c, 114, u16(150), u16(375)) },
	"death":           func(c *client) { feed(c, 83) },
	"option_menu":     func(c *client) { feed(c, 245, u8(2), str("Yes please"), str("No thanks")) },
	"system_update":   func(c *client) { feed(c, 52, u16(500)) },
	"npc_damaged":     func(c *client) { feed(c, 104, u16(1), u16(412), u8(2), u8(5), u8(15), u8(20)) },
	"npc_message":     func(c *client) { feed(c, 104, u16(1), u16(412), u8(1), u16(7), str("Hello there")) },
	"player_damaged":  func(c *client) { feed(c, 234, u16(1), u16(7), u8(2), u8(3), u8(7), u8(10)) },
	"npc_projectile":  func(c *client) { feed(c, 234, u16(1), u16(7), u8(3), u16(1), u16(412)) },
	"sleep_word_sent": func(c *client) { c.emit("sleep_word_sent", struct{}{}) },
	"kill_signal":     func(c *client) { c.events.publish("kill_signal", killSignalPayload()) },

	"npc_spawned":   func(c *client) { c.emitNPC("npc_spawned", c.npcsServer[412]) },
	"npc_despawned": func(c *client) { c.emitNPC("npc_despawned", c.npcsServer[412]) },
	"object_spawned": func(c *client) {
		c.emitObject("object_spawned", &object{id: 18, x: 130, z: 512, dir: 0})
	},
	"object_despawned": func(c *client) {
		c.emitObject("object_despawned", &object{id: 18, x: 130, z: 512, dir: 0})
	},
	"wall_object_spawned": func(c *client) {
		c.emitWallObject("wall_object_spawned", &wallObject{id: 2, x: 130, z: 512, dir: 1})
	},
	"wall_object_despawned": func(c *client) {
		c.emitWallObject("wall_object_despawned", &wallObject{id: 2, x: 130, z: 512, dir: 1})
	},
	"ground_item_spawned": func(c *client) {
		c.emitGroundItem("ground_item_spawned", &groundItem{id: 10, x: 130, z: 512})
	},
	"ground_item_despawned": func(c *client) {
		c.emitGroundItem("ground_item_despawned", &groundItem{id: 10, x: 130, z: 512})
	},
}

func TestGoldenEventPayloads(t *testing.T) {
	for name, produce := range eventScenarios {
		t.Run(name, func(t *testing.T) {
			c := goldenClient()
			produce(c)
			evs := c.events.since(0).events
			if len(evs) != 1 || evs[0].name != name {
				t.Fatalf("scenario produced %d events (first %q), want one %s", len(evs), firstName(evs), name)
			}
			golden(t, "events/"+name+".json", evs[0].data)
		})
	}

	t.Run("session", func(t *testing.T) {
		ma := &managedAccount{key: "fellow_tst1", session: 2, events: newEventRing(4)}
		ma.setState(stateLoggingIn, "loading")
		golden(t, "events/session.json", ma.events.since(0).events[0].data)
	})
}

func firstName(evs []event) string {
	if len(evs) == 0 {
		return ""
	}
	return evs[0].name
}

// --- call responses -------------------------------------------------------------

// goldenScene gives a fake client something for every query to report: a
// position and fatigue, a tree and a stump, an axe, a log and a sleeping bag,
// a banker in view with the banker's menu showing.
func goldenScene(c *client) {
	c.x, c.z = 220, 634
	c.fatigue = 37
	for i, o := range []*object{{id: 1, x: 218, z: 630}, {id: 4, x: 219, z: 630}} {
		c.objects[i] = o
	}
	c.objectCount = 2
	for i, it := range []*item{{id: 87, amount: 1, equipped: true}, {id: 14, amount: 1}, {id: 1263, amount: 1}} {
		it.slot = i
		c.inventory[i] = it
	}
	c.inventoryCount = 3
	c.npcs[0] = &npc{id: 95, serverIndex: 12, x: 220, z: 636, currentHP: 1, maxHP: 1}
	c.npcCount = 1
	c.optionMenu[0] = "I'd like to access my bank account please"
	c.optionMenu[1] = "What is this place?"
	c.optionMenuCount = 2
	c.optionMenuVisible = true
}

// callScenario is one request whose exact response is pinned.
type callScenario struct{ method, body string }

// callScenarios answer each served function once against goldenScene, named
// after the function, plus the error envelopes the brain acts on.
var callScenarios = map[string]callScenario{
	"get_x":               {"GET", ""},
	"get_z":               {"GET", ""},
	"get_fatigue":         {"GET", ""},
	"get_objects":         {"GET", ""},
	"get_inventory_items": {"GET", ""},
	"get_empty_slots":     {"GET", ""},
	"get_npcs":            {"GET", ""},
	"is_option_menu":      {"GET", ""},
	"get_option_menu":     {"GET", ""},
	"is_bank_open":        {"GET", ""},
	"is_sleeping":         {"GET", ""},
	"send_chat_message":   {"POST", `{"message":"hello"}`},
	"answer":              {"POST", `{"index":0}`},
	"deposit":             {"POST", `{"id":14,"amount":1}`},
	"close_bank":          {"POST", `{}`},
	"use_sleeping_bag":    {"POST", `{}`},
}

// Functions whose success needs the pathfinder, which needs the landscape
// archive these tests don't load; their gone answers are pinned instead, and
// the live suite covers their success.
var pathfindingFunctions = map[string]callScenario{
	"walk_path_to": {"POST", `{"x":-1,"z":0}`}, // a bad_argument: the call shape, without walking
	"at_object":    {"POST", `{"object":"object/1@170,640"}`},
	"talk_to_npc":  {"POST", `{"npc":"npc/13:id=95"}`},
}

func TestGoldenCallResponses(t *testing.T) {
	e := newTestEnv(t, "")
	e.fakeOnline("fellow", fakeOpts{setup: goldenScene})

	run := func(file, name string, sc callScenario) {
		t.Run(name, func(t *testing.T) {
			_, raw := e.raw(sc.method, "/v1/accounts/fellow/fn/"+name, sc.body)
			golden(t, file, raw)
		})
	}
	// Queries first: the actions change the scene (answer closes the menu).
	names := make([]string, 0, len(callScenarios))
	for name := range callScenarios {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		qi, qj := callScenarios[names[i]].method == "GET", callScenarios[names[j]].method == "GET"
		if qi != qj {
			return qi
		}
		return names[i] < names[j]
	})
	for _, name := range names {
		run("functions/"+name+".json", name, callScenarios[name])
	}
	for name, sc := range pathfindingFunctions {
		run("functions/"+name+".error.json", name, sc)
	}

	t.Run("account_offline", func(t *testing.T) {
		e := newTestEnv(t, "")
		fa := e.fakeOnline("fellow", fakeOpts{})
		fa.ma.setState(stateDisconnected, "relogin pending")
		_, raw := e.raw("GET", "/v1/accounts/fellow/fn/get_x", "")
		golden(t, "errors/account_offline.json", raw)
	})
}

// Every served function has a golden scenario, so a new one can't ship
// without its response pinned.
func TestGoldenCoversEveryFunction(t *testing.T) {
	var missing []string
	for name := range functions {
		_, ok := callScenarios[name]
		_, pathfinding := pathfindingFunctions[name]
		if !ok && !pathfinding && name != "test_panic" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("functions with no golden scenario: %v", missing)
	}
}

// Every streamed event has a golden scenario, so a new hook can't ship
// without its payload pinned. server_tick is built inline by the stream
// (it isn't in the ring) and is covered by TestEventsFilterTicksAndSessions.
func TestGoldenCoversEveryEvent(t *testing.T) {
	var missing []string
	for _, name := range gameEvents {
		if _, ok := eventScenarios[name]; !ok && name != evTick {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("events with no golden scenario: %v", missing)
	}
}
