# Plutonium (fellowship fork)

This is `plutonium-serve`, a fork of [plutonium](https://gitlab.com/openrsc/plutonium) for Project Fellowship. It adds a serve mode (`-serve`) that exposes plutonium's script API over JSON REST, so an external program can drive accounts with no Python script involved. [SERVE.md](SERVE.md) documents serve mode, and [GPYTHON.md](GPYTHON.md) is a reference for anyone writing a Python chore script.

**About the code in this fork:** the changes this fork makes on top of upstream — serve mode, the HTTP adapter, and related edits — are almost entirely AI-generated, written with [Claude Code](https://claude.com/claude-code) under the repository owner's direction and review. The plutonium client itself (protocol, pathfinding, scripting engine) is upstream's work and is unchanged apart from small hooks the adapter needs. Use `git log` against upstream to see exactly which parts are which.

### Testing

```bash
go test -race ./...                                  # always: unit, HTTP component and fake-game-server tests, no real game server needed
go test -race -tags integration -run Live ./...      # opt-in: live test against the local OpenRSC server
go test -run '^$' -fuzz FuzzAccountKeyMatchesServer -fuzztime 30s .   # occasional: fuzz account keys against the server's username hash
go test -run Golden -update ./...                    # after an intended JSON shape change: rewrite testdata/, then review the diff
```

The live test needs the game server running on `127.0.0.1:43594` and reuses one test account (`Fellow Tst1` by default; override with `PLUTONIUM_IT_USER`, `PLUTONIUM_IT_PASS`, `PLUTONIUM_IT_GAME`). The race detector matters here: serve mode relies on only each account's event loop touching that account's game state.

The rest of this README is upstream's and describes plutonium itself.

---

# Plutonium

Plutonium is a lightweight shell bot written for the [OpenRSC](https://rsc.vet/) Uranium server that runs on at least Linux, Windows, and MacOS. A shell bot is a bot that runs without any graphics and thus has a much smaller memory and cpu footprint than other bots. A Plutonium process running 10 accounts uses around 25-30mb memory. Plutonium also makes it easy to run several accounts at once with a single command, and doesn't have the inherent memory leaks of traditional RSC clients - meaning it can run for years on a stable box. 

Scripts for it are written in Python 3.4 via [gpython](https://github.com/go-python/gpython).

Consider that the OpenRSC servers have a max player per ip limit. At the time of this writing the limit is 10 on Uranium. There is also a registration limit per day so you need to spend multiple days creating accounts in order to reach the maximum of 10.

Some code is ported from Java to Go from the Open RSC server (in particular, the pathfinding and chat message code).

## Download

Download pre-built binaries [here](https://gitlab.com/openrsc/plutonium/-/releases).

## Account Configuration

To run Plutonium you need account files. Each account file specifies the username and password of an account and the script to run for that account. An account config is created by creating a file in the `accounts` directory. These files must be in [toml](https://toml.io/en/) format.

To get started quickly, rename the example file in the `accounts` directory to be `myaccount.toml`. Then edit in your username and password while making sure to change the script and script settings. Windows users should note that [file name extensions](https://answers.microsoft.com/en-us/windows/forum/all/how-can-i-get-the-extension-to-display-along-with/ec523f53-357b-41eb-a6c7-9b6b95a91235) must be enabled to rename the file properly.

Example file:

```toml
[account]
user = "myuser"
pass = "mypass"
autologin = true
enabled = true
debug = true

[script]
name = "varrock_east_miner.py"
progress_report = "20m"

[script.settings]
ore_type = "iron"
powermine = false
```

Feel free to change the `debug` field to `false` if you're running several accounts and it's spamming the console.

Progress reports will be written to `logs/progress_reports` every interval you set for the `progress_report` field. Though only when the script you're running is designed to generate progress reports. The progress report field can contain values that `time.ParseDuration` in Go can parse. Check [here](https://pkg.go.dev/time#ParseDuration) for the list of possible suffixes.

## Run Plutonium

On Linux open a terminal, or on Windows open a command prompt. Note that for the rest of the README if a command starts with `./plutonium` then on Windows you would replace that with `.\plutonium.exe`.

Make sure your working directory in your terminal is the `plutonium` directory and run:

```bash
./plutonium
``` 

This will run all of the accounts that are defined in your `accounts` directory.

To close Plutonium press Ctrl+C in the terminal/command prompt.

You can list command line arguments by running `./plutonium -h`.

If you want to run just one instance of a bot for any reason, for example to skip the tutorial you can do:

```bash
./plutonium -u myuser -p mypass -s scripts/skip_tutorial.py
```

You can't specify script settings with this method. Use the `-l` flag in order to set autologin to true when running the bot this way.

You can alternatively run a single account file like this:

```bash
./plutonium -f /path/to/account_file.toml
```

After you create your account and run `skip_tutorial.py` you may want to run `get_bag.py` to get a sleeping bag.

## Overwrite Progress Reports

If you want progress report files to be overwritten every time they're generated you can set `overwrite_progress_reports` to `true` in `settings.toml`.

## Building Plutonium

Make sure you have at least version 1.22 of [Go](https://go.dev/) installed, along with [git](https://git-scm.com/). On Linux open a terminal, or on Windows open a command prompt. Then clone this repo with 

```bash
git clone https://gitlab.com/openrsc/plutonium
```

Enter the directory it creates (type `cd plutonium` to get in the directory) and type `go build`. A binary named `plutonium` (or on Windows `plutonium.exe`) will now be in the directory.

## Memory

Known issue: If you see your memory going up, your script is probably calling `calculate_path_to` without a `depth` or `max_depth`.

If you happen to notice that the VIRT memory allocated is high, it is due to [Go reserving a lot of virtual memory for allocations](https://go.dev/doc/faq#Why_does_my_Go_process_use_so_much_virtual_memory). Check the RES memory of `top` to see the real memory footprint.

## Scripts

General scripting documentation/features can be found [here](https://gitlab.com/openrsc/plutonium/-/blob/main/SCRIPTING.md), and API documentation can be found [here](https://gitlab.com/openrsc/plutonium/-/blob/main/API.md).