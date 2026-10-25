# Meta Keyguard

A chrome extension and CLI tool to manage your Meta API keys.

![](./banner.png)

Meta Keyguard gives Meta Model API keys an expiry date. The Meta developer dashboard has no
API for managing keys, no key expiry, and no way to switch a key off, so Keyguard does that
work through a Chrome extension that operates the dashboard on your behalf. A command-line
tool in WSL stores the keys, enforces their time to live (TTL), rotates them, and can revoke
them when [Meta Cost Center](https://github.com/omenking/meta-costcenter) reports that a
spending limit has been exceeded.

Keyguard is a single-user tool. It runs on one machine, manages one dev.meta.ai team, and
keeps everything local.

## How it works

```
 WSL                                        Windows
 keyguard CLI ──Unix socket──► keyguard bridge ◄──native messaging──► Chrome extension ──► dev.meta.ai
      │                             │  (started by Chrome through wsl.exe)
      └──── key store (SQLite) ◄────┘  runs the reaper every minute
```

The extension makes the same requests the dashboard makes when you click its buttons, from
inside a dev.meta.ai tab, so it uses your existing signed-in session. Chrome starts the
bridge through `wsl.exe` when the extension loads and keeps it running while Chrome is open.
While it runs, the bridge relays CLI requests to the extension and runs the reaper once a
minute, which deletes expired keys, finishes rotations, and applies Cost Center limits.

Because dev.meta.ai refuses to delete a team's last key, Keyguard creates a **sentinel**
key whenever a deletion would otherwise remove the last one. The extension drops the
sentinel's secret before it leaves the browser, so nobody holds it, and every other key can
then be deleted.

Keys are named `<label>-kg<timestamp>` on dev.meta.ai, for example `dev-kg20261025143000`.
You refer to them by label.

## Requirements

- WSL 2 with Windows interop enabled (the default)
- Google Chrome 116 or newer on Windows, signed in to https://dev.meta.ai/

## Install

Keyguard is a single static binary with the extension built into it. In WSL, install the
latest release into `~/.local/bin`:

```bash
curl -fsSL https://raw.githubusercontent.com/omenking/meta-keyguard/main/install.sh | sh
```

If Go is installed, `go install github.com/omenking/meta-keyguard/cmd/keyguard@latest`
builds the same binary into `~/go/bin` instead. Either way, register the bridge with Chrome:

```bash
keyguard install-bridge
```

`install-bridge` copies the extension to `%LOCALAPPDATA%\MetaKeyguard\extension`, writes the native
messaging host manifest and a small launcher batch file next to it, and registers the host
under `HKCU\Software\Google\Chrome\NativeMessagingHosts`. Then, in Chrome:

1. Open `chrome://extensions` and turn on **Developer mode**.
2. Click **Load unpacked** and choose the folder that `install-bridge` printed.
3. Run `keyguard doctor` in WSL.

The extension has a fixed ID (`admjfgnffhpgjnnbnkogihlnpeeeenio`) so that the host manifest
can allow it in advance. The launcher Chrome runs points at the binary's own path, and
`install-bridge` also records your `PATH` for the bridge, because Chrome starts it with an
almost empty one and it needs to find Cost Center. After upgrading keyguard, or moving the
binary, run `install-bridge` again and click the extension's reload button; `doctor` tells
you when the installed extension, the launcher, or the running bridge is out of date.
`keyguard uninstall-bridge` removes the registry entry and the installed files.

## Using keys

```bash
keyguard create dev                    # a key that lives for 12 hours
keyguard create ci --ttl 2h            # a shorter one
keyguard create agent --auto-rotate    # replaced automatically 10 minutes before it expires
keyguard exec dev -- python app.py     # run a program with META_API_KEY set
eval "$(keyguard env dev)"             # export META_API_KEY into this shell
keyguard get dev                       # print the secret
```

`exec` is the recommended way to use a key, because the secret then exists only in the
environment of the program that needs it. `--var NAME` sets a different variable, and can be
repeated. Keyguard cannot protect a key that you copy into a `.env` file or another
program's configuration, since that copy outlives the key's TTL.

`get`, `env`, and `exec` refuse to hand out a key that has passed its expiry, even if it has
not been deleted yet.

## Managing keys

| Command | What it does |
| --- | --- |
| `status [--json]` | Live keys, time left, overdue deletions, the last failure, and the Cost Center state. Exits with 1 when something needs attention. `--json` is the secret-free form Cost Center reads. |
| `list [--all] [--history]` | Stored keys; `--all` also lists keys on dev.meta.ai that Keyguard did not create, `--history` includes revoked keys. |
| `rotate <label> [--grace 5m]` | Creates a replacement and deletes the old key after the grace period. |
| `extend <label> --ttl 4h` | Sets a new expiry, measured from now, up to `max_ttl`. |
| `revoke <label>` | Deletes a key now. It also accepts the dev.meta.ai name or ID of a key Keyguard did not create. |
| `kill [--yes]` | Deletes **every** key on the team, including ones Keyguard did not create, leaving only a sentinel. |
| `sync` | Marks stored keys that no longer exist on dev.meta.ai as revoked. |
| `reap` | Runs the reaper once, now. |
| `log` | The audit log. Secrets are never written to it. |
| `doctor [--inspect]` | Checks every part of the setup; `--inspect` prints the redacted shape of dev.meta.ai's responses. |

## Settings

`keyguard config` shows the settings, and `keyguard config set <name> <value>` changes
one.

| Setting | Default | Meaning |
| --- | --- | --- |
| `default_ttl` | `12h` | Lifetime of a new key when `--ttl` is not given. |
| `max_ttl` | `7d` | The longest TTL `create` and `extend` accept, so that a typo cannot create a long-lived key. |
| `rotate_grace` | `5m` | How long a rotated key keeps working. |
| `rotate_before` | `10m` | How long before expiry an `--auto-rotate` key is replaced. |
| `env_var` | `META_API_KEY` | The variable `env` and `exec` set. |
| `cost_center` | `off` | Whether Keyguard checks Cost Center (see below). |
| `cost_center_command` | `muse-cost` | How to run Cost Center. |
| `team_id` | (empty) | Which team to manage, needed only if your account has several. |
| `project_id` | (empty) | Which project new keys belong to, needed only if Keyguard cannot work it out. |

Durations are written as `90s`, `30m`, `12h`, `1d`, or combinations such as `1d12h`.

## Cost Center integration

In Meta Cost Center, an alert can be set to revoke Meta keys when it passes its limit, with
a key match that selects which keys: `*` for every key, `bootcamp-*` for a prefix, or
`key1,key2` for specific keys. The match is compared with Keyguard labels, and with the
dev.meta.ai name of keys Keyguard did not create.

Keyguard only acts on those alerts when its own check is turned on:

```bash
keyguard cost-center on      # turns the check on and runs it once
keyguard cost-center check   # shows which alerts are revoking keys
keyguard cost-center off
```

Keyguard runs `muse-cost` from your `PATH`, which is where Cost Center's installer puts it.
If it lives somewhere else, name it with `keyguard config set cost_center_command /path/to/muse-cost`.

With the check on, the reaper runs `muse-cost alert status --json` every minute. For each
alert that has passed its limit and is set to revoke keys, it deletes every matching key, and
`create` refuses new keys that the alert covers until the alert's period ends or you pass
`--ignore-cost-center`. If Cost Center cannot be run, `create` also refuses to hand out new
keys, because it cannot tell whether a limit has been passed.

Cost Center's **Alerts** page shows Keyguard's side of this: whether each alert is enforced,
which keys its match covers, and what Keyguard revoked for it. It reads that from
`keyguard status --json`, which holds no secrets, so set `keyguard_command` in Cost
Center's configuration if `keyguard` is not on your `PATH`. The list of keys Keyguard did
not create is a snapshot that the reaper refreshes every ten minutes while the check is on,
and that `keyguard list --all` refreshes at once.

Cost Center only sees Muse usage recorded on this machine, so this protects against local
overspending, not against usage from elsewhere. The limit is checked at most once a minute
and Cost Center's estimates lag slightly behind real usage, so spending can pass the limit
by a small amount before the keys are deleted.

## When things fail

Keyguard is designed so that a failure is always visible rather than silent. The CLI exits
with status 2 and explains what to do, the failure is recorded and shown by `status` and
`doctor`, and the extension shows a red badge and a desktop notification. The common causes
are:

| Code | Meaning |
| --- | --- |
| `bridge_offline` | Chrome is closed, or the extension is not loaded or connected. |
| `not_logged_in` | Sign in to dev.meta.ai in Chrome. |
| `reauth_required` | dev.meta.ai wants you to confirm your identity; open the API keys page and complete the prompt. |
| `endpoint_changed` | dev.meta.ai's internal API no longer looks the way Keyguard expects. |
| `secret_not_found` | A key was created but its secret could not be found, so Keyguard deleted it again. |

The last two mean Meta has changed its dashboard. Everything that depends on its internal
API is in `extension/adapter.js`, and `plans/RECON.md` describes how that API was worked out
and what is still unconfirmed.

### Limits of TTL enforcement

Meta has no key expiry, so a key only stops working when something deletes it. The bridge
deletes expired keys while Chrome is open. If WSL is not running, the extension deletes
them itself. If Chrome is closed, the computer is asleep, or the dev.meta.ai session has
expired, nothing can delete them. Such keys are reported as **overdue** by `status` and in
the extension, and they are deleted at the first opportunity after Chrome starts again.

## Where data is stored

| What | Location |
| --- | --- |
| Key store, settings, and audit log | `~/.local/share/meta-keyguard/keys.sqlite3` (directory `0700`, file `0600`) |
| Bridge log | `~/.local/share/meta-keyguard/bridge.log` |
| Windows side | `%LOCALAPPDATA%\MetaKeyguard` |

The data directory follows `$XDG_DATA_HOME`. Secrets are stored in plain text in the key
store, protected by file permissions in the same way as `~/.aws/credentials`, and are
removed from the store as soon as a key is revoked.

## Development

```bash
go build -o keyguard ./cmd/keyguard     # embeds extension/ into the binary
go test ./...                           # CLI, store, reaper, and an end-to-end bridge test
node --test extension/tests/            # the dev.meta.ai adapter
```

Releases are built by GoReleaser from `.goreleaser.yaml` (`goreleaser release` on a tag),
which publishes the `keyguard_linux_amd64.tar.gz` and `keyguard_linux_arm64.tar.gz`
archives that `install.sh` downloads. Changes to `extension/` reach users only through a
new binary, since the extension is embedded in it.

The bridge tests start a real `keyguard bridge` process and play the extension's side of
native messaging. The adapter tests run against a pretend dev.meta.ai that enforces the
last-key rule. Neither touches the real dashboard.
