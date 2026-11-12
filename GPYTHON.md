# gpython reference

For anyone writing a Python chore script (account setup, one-offs) for this fork. The
fellowship bots themselves don't use scripts; they drive accounts through serve mode
([SERVE.md](SERVE.md)). Pinned gpython version: `v0.2.1-0.20250704170033-bbe47265af95`,
roughly **Python 3.4 semantics**.

## How this was established

1. **Compile probes:** `plutonium -compile <file>`, one tiny file per feature.
2. **Reading gpython's module registry** in the Go module cache. This was necessary
   because **`import` compiles to an opcode without resolving the module**, so a clean
   `-compile` on `import json` proves nothing about runtime. Don't repeat that mistake.

## Language

**Works:** classes, inheritance, `super()`, `@property`, `@staticmethod`, decorators,
generators, `with`, comprehensions (including dict), `try/except ... as`, **function**
annotations, and dotted imports (`import mybot.nav.router` → `mybot/nav/router.py`,
`py/import.go:103-104`).

**Fails to compile:** f-strings, walrus (`:=`), variable annotations (`x: int = 5`). Use
`.format()` or `%`.

## Standard library

The complete list:

```
array  binascii  builtin  glob  marshal  math  os  string  sys  tempfile  time
```

**Absent:** `json`, `re`, `random`, `collections`, `dataclasses`, `enum`, `abc`,
`typing`, `socket`, `threading`, `subprocess`, `itertools`, `functools`, `datetime`,
`logging`. Builtins include `open()` and `os.system`; `os` provides `listdir`, `remove`,
`mkdir`, `getpid`, `getenv`, `system`. Plutonium supplies its own `random(min, max)`.

## Sharp edges

- **Relative imports raise `SystemError`** (`"Relative import not supported yet"`,
  `py/import.go:99`). Every import must be absolute.
- **Dicts take string keys only** (per `SCRIPTING.md`): `d[str(k)] = v`.
- **Module-level code cannot call the API.** `addGlobalsToAll` (`script.go:825`) injects
  the ~200 globals and `settings` *after* module initialisation, so every API call must
  be inside a function body, as in `scripts/millz_common.py`, which keeps only inert
  data at module level. Otherwise: a baffling `NameError`.
- **Unverified:** `addGlobalsToAll` (`script.go:825-835`) finds submodules by recursing
  into module-valued globals, so a module reached only via
  `from mybot.nav import router` may never receive the API globals. Reasoned from
  source, **not tested**: keep the module graph flat, or test before going deep.

## Where scripts can live

A script need not live in this tree: `-s <path>` is used verbatim and its directory goes
on `sys.path` (`client.go:452-456`, `script.go:199-204`), with `scripts/` appended, so an
external script can still import `millz_common`. Account `.toml` files resolve `name`
relative to the configured scripts directory (`config.go:168`), so `-s` is the cleaner
route for anything outside the repo.

The bundled scripts include useful one-offs (`skip_tutorial.py`, `get_bag.py`,
`get_pick.py`) and shared helpers (`millz_common.py`, with its bank coordinate table and
experience curve).
