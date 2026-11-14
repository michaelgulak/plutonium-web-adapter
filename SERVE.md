# Serve mode: plutonium's API over HTTP

Serve mode (`-serve`) runs plutonium as an HTTP server that a separate program, the
Kotlin `fellowship-brain`, uses to drive accounts. It serves **plutonium's script API as
it already exists** ([API.md](API.md)): same names, parameters and returned objects,
scoped to an account. It holds **no logic** of its own; no Python or gpython is involved
anywhere on this path, since handlers call the `*client` methods directly. Why it is
shaped this way is in the manifesto's ARCHITECTURE.md (§5, the network adapter).

## Running it

From this directory (plutonium finds `settings.toml` and `data/` next to its binary):

```bash
go build -o plutonium .
./plutonium -serve 127.0.0.1:8787
```

It must bind to a loopback address. Setting `PLUTONIUM_API_TOKEN` makes every request
require `Authorization: Bearer <token>`.

`settings.toml` points at the local server (`127.0.0.1:43594`), and `data/RSA.json`
holds the modulus from the server's `client.pem`. The server only regenerates its
keypair if that file is deleted (`Crypto.java:96-115`); if it ever is, copy the new
modulus in. If `client_version` ever disagrees, change plutonium's setting, never the
server's.

Other useful flags (`client.go:354-365`): `-r <path>` bulk account registration,
`-s <path>` run a single script, `-compile <path>` parse without connecting, `-f` run
one account file, `-d` download RSA keys.

## Calls

Every script function is served under its **verbatim name** at
`/v1/accounts/{account}/fn/{function}`, driven by one registry table. Queries
(inventory, nearby NPCs, position) are `GET`s with arguments in the query string;
actions (walk, attack, withdraw) are `POST`s with a JSON body. Resource-style routes may
be added later as aliases, but the verbatim names stay canonical. Arguments are
validated against the registry, and an unknown or missing argument is an error, so a
typo on the caller's side fails loudly instead of being ignored.

The surface grows one slice at a time, as the brain needs it (FELLOW-11, adapter call
surface). Served so far:

| Slice | Functions |
|---|---|
| first calls | `get_x`, `get_z`, `get_fatigue`, `send_chat_message` |
| woodcutting | `walk_path_to`, `get_objects`, `at_object`, `get_inventory_items`, `get_empty_slots` |
| banking | `get_npcs`, `talk_to_npc`, `is_option_menu`, `get_option_menu`, `answer`, `is_bank_open`, `deposit`, `close_bank` |
| sleeping | `is_sleeping`, `use_sleeping_bag` |
| area transitions | `get_wall_objects`, `at_wall_object`, `at_object2` |
| path queries | `calculate_path_to`, `calculate_path` |
| replying to chat | `walk_to` |

- `walk_path_to` answers `false` for "no path". On that precedent, `use_sleeping_bag`
  answers `true` when a bag was carried and used, and `false` when none was, in which
  case nothing is sent (plutonium itself returns nothing and prints a message only this
  process's console would show).
- `answer` checks the index against the menu's real option count.
- `walk_to` sends one walk packet planned within the loaded region, with none of
  `walk_path_to`'s long-distance search. Its use here is stopping: sent to the tile the
  player stands on, the server drops whatever path it was walking
  (`WalkRequest.java:94-95`), so the brain can stop a bot before it types a reply.
- `at_wall_object` takes its ref as `wall_object`, plutonium's parameter name. A wall
  object's ref carries its direction, since a tile can hold one per edge.
- With no script running, plutonium answers the sleep word itself, but only once the
  sleep fatigue reaches zero (`client.go:2283`), so a serve-mode sleep always runs to 0%.
  The answer is announced as the `sleep_word_sent` event. When plutonium's hash table
  doesn't know the sleep image it sends `unknown`, and the server, not knowing the answer
  either, accepts any word (`SleepHandler.java:64`).
- `open_bank()` is deliberately not served: it is a script routine with its own timer,
  not a primitive, so the brain composes it from `talk_to_npc` and `answer`.
- `calculate_path_to` and `calculate_path` are queries. Plutonium returns a path
  object that walks a cursor along the path and exists only in this process, so they
  return its read-only side instead, as a value: `{"length": n, "coords": [{"x","z"}, …]}`,
  the tiles after the start up to and including the target, or `null` for no path within
  `max_depth`. The arguments are plutonium's: `depth`, `skip_local` and `max_depth` are
  optional. `skip_local` defaults to **false**, as plutonium's code does
  (`script.go:1051`), although API.md says true. With it set, only the landscape and
  fixed scenery count, so a closed door or gate in view doesn't block the path.
- A path that starts where it ends is refused as `bad_argument`, because plutonium would
  loop forever sizing its search (`accessors.go:584-597`). So are a `depth` outside 1 to
  800 and a `max_depth` outside 1 to 512 (API.md's limits). On the real landscape, a
  query with no path from open ground takes about 5 ms at `max_depth` 64 and about
  180 ms at 512, all of it on the account's event loop.
- Plutonium's other path calls (`calculate_path_through`, `create_path`, and the path
  object's walking methods) aren't served, since their state lives in this process.

## Entity references

Entities carry a deterministic natural-key `ref`: `npc/412:id=65`, `player/<key>`,
`object/18@130,512`, `wall/2@130,512:1`, `ground/10@130,512`, `inv/4`. There is no
registry of handles: a ref is resolved against live state inside the call's closure and
its embedded type id is checked. A ref whose coordinates now hold a different object (a
tree turned stump), an NPC index now held by another type of NPC, or nothing, answers
`410 entity_gone`, so a reused NPC slot is never acted on by mistake. Refs travel in JSON
bodies and query strings, never in URL path segments.

## Accounts

Serve mode starts with **no accounts**; the caller owns the credentials and drives the
lifecycle over HTTP.

- `POST /v1/accounts` registers an account on the game server. The server rate-limits
  registration, which is reported as `429` with `Retry-After` rather than waiting five
  minutes inside a request.
- `POST /v1/accounts/{account}/login` answers `202` at once. Logins are staggered six
  seconds apart process-wide, so the outcome is read from the account's **state**:
  `logging_in` → `online` (once the world has loaded) → `disconnected` (relogin pending)
  or `stopping` → `stopped`. A fatal failure (bad credentials, outdated client,
  permanently disabled) ends in `login_failed` with a reason and is not retried;
  transient ones (world full, too many attempts) keep retrying.
- `POST /v1/accounts/{account}/logout` sends the logout packet. The server refuses it
  right after login and while in combat, so the socket is closed after ten seconds
  regardless. SIGTERM logs every account out before the process exits.
- Every **successful** response carries a `session` counter that increments on each
  login, so the caller can tell a relogin happened and drop anything it held from before.
  Error responses don't carry it (a `409` carries the account's `state` instead).

Credentials are encoded exactly as the original client does (non-alphanumerics become
`_`, passwords are cut to 20 characters), the authentic RuneScape behaviour, kept
deliberately. They are held in memory only and never logged.

**Addressing.** `{account}` is a canonical key reproducing the server's own username
equivalence: `DataConversions.usernameToHash` applied to the client's wire encoding.
That means lowercase ASCII, every other byte a `_`, separators trimmed at both ends, runs
of separators kept (the server hashes each one), and at most 12 characters. Any spelling
is accepted (`Mike%20Gulak`, `mike-gulak`) and responses return the canonical `key`.
Longer names are rejected rather than truncated, because a truncated key could end in
`_` and canonicalise differently when sent back. A fuzz test checks the equivalence
against a Go port of the server's hash.

## Concurrency: the call channel

Each account's `*client` state is touched only by its own event-loop goroutine, with no
locks. HTTP handlers never read it directly: they send a closure on a per-account **call
channel** that the event loop serves between packets, and wait for the result, the same
pattern as a UI main-thread dispatcher. A lookup and the action that depends on it run
in the same loop turn, so the world cannot change between them.

## Errors

One envelope: `{"error":{"code":"…","message":"…", …}}`. Anything the caller can recover
from is a **4xx**: `400` bad argument, JSON or username; `404` unknown account or
function; `405` wrong method; `409` account not online (with its state), already
running, or username taken; `410` entity gone; `429` registration rate limit. A **5xx**
means changing the request cannot help: `500` an adapter bug (a recovered panic), `502`
an unknown game-server failure, `504` an event loop that didn't answer within ten
seconds.

## Events

Script hooks are pushed by the game, so they reach the caller as **server-sent events**,
one stream per account: `GET /v1/accounts/{account}/events`. Event names are the hook
names without `on_` (`chat_message`, `death`, `kill_signal`), and payloads are value
copies taken at the moment of the hook, using the entity refs above. Three hooks are not
streamed: `on_load` is the `session` event going `online`, and `on_init`,
`on_bot_message` and `on_progress_report` belong to the script engine, which serve mode
doesn't run. Two events are serve mode's own: `session`, published on every state change
with the state, reason and session number, and `resync`.

**Events are hints; state queries are the truth.** Each account keeps a ring buffer of
its recent events (4096 by default, `-event-buffer`), numbered per account rather than
per login, so resuming survives a relogin. Each subscriber is a cursor into the ring,
reading at its own pace, so the game loop never waits for a reader and nothing grows
without bound.

- **Resuming.** A reconnect sends `Last-Event-ID` and resumes from the ring. No header
  means "from now on"; `0` replays everything the ring holds, which is how a caller sees
  everything since the login it just requested (an account only exists once its first
  login is posted). Ids carry a per-process prefix, so an id from before a restart isn't
  mistaken for a current one.
- **Falling behind.** If a reader's next event has been overwritten (a paused debugger
  is the realistic case), or its id is from an earlier process, it receives `resync` and
  should re-query the state it cares about.
- **Sessions.** The session number increments when the connection succeeds, before the
  world loads, so spawn events sent while loading already belong to the new session:
  `session` (`logging_in`, `loading`, session N) → spawns → `session` (`online`, N).
- **Filtering and ticks.** `?events=a,b` sends only the named events; `session` and
  `resync` are always sent. `server_tick` is opt-in and kept out of the ring: at one per
  640 ms it would flush everything else out, and only the latest tick matters for
  pacing, so it arrives coalesced and without an id.
- **Heartbeat.** A comment every 15 seconds exposes dead connections. For a filtered
  reader it also carries the id of the events it skipped, so a reconnect doesn't reread
  them.

**Shutdown.** SIGTERM is serve mode's `on_kill_signal`. New logins get `503`, every
running account receives `kill_signal`, and a listening caller has 15 seconds, the same
window plutonium gives scripts, to log its accounts out itself. Whatever is still
running is then logged out here. The HTTP server stays up throughout, because the caller
needs it to log out; the event streams end last.

**Known weakness:** the ring holds every event, so a flood of spawns can push out a chat
message even for a reader filtering to chat, and chat can't be recovered by querying. If
that shows up in practice, the planned fix is a separate, longer-lived lane for
unrecoverable events (chat, private messages, death, trade requests).

## Tests

The commands are in [README.md](README.md#testing). The race detector is part of every
run because the call channel rests on only the event loop touching an account's state.
The **fake game server** (`fakegame_test.go`) speaks just enough protocol for the real
client to log in, so the always-on tests drive the real session goroutine, event loop
and packet handlers through login, events, calls, logout, relogin and shutdown,
deterministically and in about a second. The live suite reuses one test account because
the server rate-limits registration; it also checks real spawn events and the
`kill_signal` handshake.

**Golden files** pin the exact JSON, compared byte for byte: one indented file per
scenario under `testdata/`, named after the verbatim event or function name. They are
plain example payloads, so the brain's wire primitives and stub adapter can adopt them
as a shared contract later.

- *Events:* every streamed event has one, produced through the real packet handlers
  where possible.
- *Calls:* every served function has one under `testdata/functions/`, answered through
  the real HTTP handlers. The path queries run on a map with no landscape loaded, which
  is open ground. `walk_path_to`, `walk_to`, `at_object`, `at_object2`, `at_wall_object` and
  `talk_to_npc` need the pathfinder, which needs the landscape archive the tests don't load, so their files pin
  an error answer instead, and the live suite covers their success.
- *Errors:* `testdata/errors/` pins the `account_offline` envelope.

A test fails if an event or function ships without its file. Because `-update` accepts
whatever the code now produces, a commit that changes golden files lists which shapes
changed and why, so review reads a summary rather than raw diffs.
