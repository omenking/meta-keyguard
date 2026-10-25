# Meta Keyguard — Main Plan

## Business Use Case

Our organization plans to use the Meta Model API for an upcoming project because of its
competitive API pricing. However, Meta lacks mature API key management, spending limits,
and notification safeguards, and I have been tasked with implementing safeguards against
runaway billing and long-lived API keys.

Known Meta Model API limitations, based on my observations of https://dev.meta.ai/ and use
of the platform:

- There are no official endpoints to manage API keys.
- You cannot delete every key, because one must always remain. Rotating the last key
  therefore requires creating a new key first.
- Keys cannot be activated or deactivated.
- Keys cannot be given an expiry, so every key is long-lived.
- Billing is pay-as-you-go rather than prepaid, so a runaway bill is possible.
- There is no hard spending limit at either the account or the key level.
- A billing notification can be set for a dollar amount, but:
  - only one notification can be set,
  - it cannot be expressed as a percentage of a target cap,
  - it applies to the whole account, and
  - there are no per-key notifications.

I have already built [Muse Cost Center](https://github.com/omenking/meta-costcenter),
which collects local Muse usage in WSL, estimates its cost, and supports richer spending
alerts than Meta provides.

## Technical Goal

Meta Keyguard is a two-part tool:

- A **Chrome extension** that can list, create, and delete keys on dev.meta.ai.
- A **local CLI** that stores keys centrally and enforces policy on them:
  - it can rotate a key,
  - keys have a default TTL of 12 hours, and
  - the TTL can be set per key.

Two constraints shape the design. First, the CLI runs in WSL while Chrome runs on the
Windows side, so the two halves need a bridge across that boundary. Second, when Muse Cost
Center is installed, Keyguard should be able to revoke keys once a spending limit is
reached.

## Status (2026-10-25)

Phases 0 to 4 are built, and the decisions below record the answers to the open questions.
Everything has been tested except the extension running inside Chrome against the real
dashboard, which needs the extension loaded in your browser; `plans/RECON.md` lists what that
first run will confirm. Where the build differs from the original design, the sections below
have been updated to describe what was built.

---

## Design

### Overview

```
 WSL                                         Windows
┌─────────────────────────────┐            ┌──────────────────────────────┐
│ keyguard CLI                │            │ Chrome                       │
│   ├─ key store (SQLite)     │  bridge    │  └─ Keyguard extension (MV3) │
│   ├─ reaper (TTL enforcer)  │◄──────────►│      ├─ service worker       │
│   └─ cost-center hook       │            │      └─ content script on    │
│                             │            │         dev.meta.ai          │
│ muse-cost (Cost Center)     │            │                              │
└─────────────────────────────┘            └──────────────────────────────┘
```

The CLI owns all state and policy: which keys exist, who they belong to, when they expire,
and what should happen when a limit is reached. The extension is deliberately thin. It
performs the three operations that only a logged-in browser session can perform (list,
create, delete), reports the results back, and never persists a key secret. Because Meta
displays a new key's secret only once, at the moment of creation, the extension must
capture it from that response and hand it straight to the CLI.

### The WSL ↔ Windows bridge

There are two workable ways to connect the CLI to the extension.

**Option A: Chrome native messaging (recommended).** Chrome launches a native host that it
finds through a registry entry under
`HKCU\Software\Google\Chrome\NativeMessagingHosts\<name>`. On Windows the host can be a
small `.bat` file that runs `wsl.exe -d <distro> keyguard bridge`, and Chrome's
stdin/stdout framing passes through `wsl.exe` intact. The extension opens a long-lived
port with `chrome.runtime.connectNative()` when it starts, which also keeps the MV3 service
worker alive while the port is open. The `keyguard bridge` process then listens on a Unix
socket in WSL, so that other CLI invocations can send it requests. The main advantage of
this option is that no network port is opened at all, and Chrome only allows the
extension IDs listed in the host manifest to connect, which gives the bridge
authentication without any extra work. The cost is a one-time installation step that
writes the registry key and manifest, which `keyguard install-bridge` can perform from
WSL through `reg.exe` and `/mnt/c`.

**Option B: localhost WebSocket.** The CLI runs a server on `127.0.0.1`, which WSL2
forwards to Windows, and the extension connects to it. This avoids the registry, but any
local process or web page can attempt to reach a localhost port, so the connection would
need a pairing secret that the user copies into the extension. It also depends on WSL's
localhost forwarding, which varies between NAT and mirrored networking modes.

The plan assumes Option A and keeps Option B as a fallback if native messaging through
`wsl.exe` turns out to be unreliable in practice. Phase 0 should prove the round trip
before anything else is built on it.

### Automating dev.meta.ai

Since Meta publishes no key management API, the extension has to work through the
dashboard. There are two approaches, and the Phase 0 investigation should determine which
one is used:

1. **Internal requests.** The dashboard almost certainly calls an internal endpoint (likely
   GraphQL) to list, create, and delete keys. Replaying those calls from the page context,
   with the user's existing session, is faster and less sensitive to layout changes than
   clicking through the UI. It is, however, an undocumented interface that Meta can change
   without notice.
2. **DOM automation.** A content script drives the API keys page by clicking the same
   buttons a person would and reading the secret from the creation dialog. This breaks
   whenever the page layout changes, but it does exactly what the user could do by hand.

**Decision:** internal requests. The endpoints were found in the dashboard's public JavaScript
bundle and are documented in `plans/RECON.md`. Because they are undocumented, any response
the adapter does not recognise is reported as a named failure rather than guessed at.

Whichever approach is chosen, the extension isolates it behind a small adapter with
`listKeys()`, `createKey(name)`, and `deleteKey(id)`, so that a change on Meta's side
requires fixing one file. The extension also needs to detect when the user is logged out
or is being asked for MFA, and report that state to the CLI as a distinct error instead of
failing silently. When no dev.meta.ai tab is open, the extension opens one in the
background, performs the operation, and closes it.

Keys created by Keyguard are named `<label>-kg<yyyymmddhhmmss>`, so that the label stays
readable on dev.meta.ai (and in Cost Center key matches) while rotated keys remain distinct.

### The last-key rule and the kill switch

Because an account must always keep one key, "delete everything" is impossible as stated.
Keyguard works around this with a **sentinel key**: a key created by Keyguard whose secret
is discarded immediately and never stored anywhere. Once a sentinel exists, every other
key can be deleted, which leaves the account with exactly one key that nobody possesses.
The same idea makes rotation of the last real key possible, because the replacement is
created before the old key is deleted.

The kill switch (`keyguard kill`) therefore performs these steps:

1. Ensure a sentinel key exists, creating one if necessary.
2. Delete every managed key.
3. Delete unmanaged keys as well (decision 4).
4. Mark every stored key as revoked and write an audit entry.

### TTL enforcement

Meta has no expiry, so TTL is enforced by Keyguard itself, which means a key only expires
if something is running to delete it. That is the most important limitation of this
design, because a laptop that is asleep, a closed Chrome window, or an expired dev.meta.ai
session will each prevent deletion. To narrow that gap, enforcement happens in two places:

- **The CLI reaper** runs inside the bridge process once a minute, for as long as Chrome
  keeps the bridge alive, and deletes expired keys. A separate timer is unnecessary because
  the reaper can only act through the bridge anyway. `keyguard reap` runs it on demand.
- **The extension** keeps a copy of each managed key's ID and expiry time, but never its
  secret, and schedules a `chrome.alarms` alarm for it. This lets the extension delete an
  expired key even when WSL has shut down.

When a key is past its expiry but could not be deleted, both sides report it as
**overdue**, the CLI shows it prominently in `keyguard status`, and the extension shows a
badge on its icon. Expiry is checked again whenever Chrome starts, so a key that expired
during sleep is deleted at the next opportunity.

The default TTL is 12 hours. The TTL can be set at creation (`--ttl 4h`), extended later
(`keyguard extend`), and capped by a configurable maximum, so that a typo such as
`--ttl 9999h` cannot recreate the long-lived key problem.

### Rotation

`keyguard rotate <label>` creates a replacement key, stores it under the same label, and
schedules the old key for deletion after a grace period (default 5 minutes), so that
processes that have already read the old key have time to pick up the new one. Rotation
can also happen automatically shortly before a key's TTL elapses, if the label is marked
`--auto-rotate`; otherwise the key simply expires.

### Delivering keys to programs

A stored key is only useful if programs can read it without being copied into files that
outlive it. The CLI offers:

- `keyguard exec <label> -- <command>`, which runs the command with `META_API_KEY` set,
- `keyguard env <label>`, which prints an `export` line for `eval`, and
- `keyguard get <label>`, which prints the raw secret for scripting.

Writing keys into `.env` files is not supported, because a rotated or expired key left in a
file is exactly the stale long-lived credential this tool is meant to eliminate. A user who
copies a key somewhere else anyway is outside what Keyguard can protect (decision 2).

### Meta Cost Center integration

Cost Center decides which keys an exceeded limit should revoke, and Keyguard decides whether
to act on it. Cost Center never runs a command itself.

On the Cost Center side, each alert gains a **Revoke Meta API keys** checkbox and a **key
match**: `*` for every key, `bootcamp-*` for a prefix, or `key1,key2` for specific keys
(`fnmatch`-style patterns, comma separated). `muse-cost alert status --json` prints every
alert with its level and these two fields, after importing any new usage.

On the Keyguard side, the check is off until `keyguard cost-center on`. When it is on, the
reaper polls `muse-cost alert status --json` every minute. For each alert whose level is
`exceeded` and which revokes keys, it deletes every key whose Keyguard label, or whose
dev.meta.ai name for a key Keyguard did not create, matches the key match. While that alert
stays exceeded, `create` refuses new keys it covers unless `--ignore-cost-center` is given.
If Cost Center cannot be run, the failure is reported by `status` and `create` refuses new
keys, on the principle that a spending safeguard should fail closed.

Two limitations of this integration should be stated plainly in the documentation. Cost
Center only sees Muse usage recorded on the local machine, so spending from keys used on
other machines or by direct API calls is invisible to it. In addition, Cost Center has no
notion of which API key was used, so a key match selects keys by name rather than by the
spending each key caused.

A possible later improvement is for the extension to read the dev.meta.ai Usage page,
which can filter by API key and export CSV. That would give real per-key spend across all
machines, although Meta labels those metrics as having limited availability and they will
lag behind real usage.

### Storage and security

| What | Location |
| --- | --- |
| Key store, settings, and audit log | `~/.local/share/meta-keyguard/keys.sqlite3` (directory `0700`, file `0600`) |
| Bridge socket | `~/.local/share/meta-keyguard/bridge.sock`, or `$XDG_RUNTIME_DIR` if that path is too long |
| Bridge log | `~/.local/share/meta-keyguard/bridge.log` |

These paths follow `$XDG_DATA_HOME`. Settings live in the database and are changed with
`keyguard config set`, because Python's standard library can read TOML but not write it. The
store holds each key's label, Meta key ID, name, creation time, expiry, state (`active`,
`retiring`, `revoked`, or `sentinel`; *overdue* is derived from the expiry), and the secret
in plain text (decision 6). Secrets are deleted from the store as soon as the key is
revoked, and never written to the audit log.

The extension requests only the permissions it needs (`nativeMessaging`, `alarms`,
`storage`, `scripting`, `notifications`, and host access to `https://dev.meta.ai/*`), loads
no remote code,
and keeps secrets only in memory for the time it takes to pass them across the bridge.

### Technology choices

The CLI is written in Python 3.11 using only the standard library, matching Cost Center so
that both tools install the same way and can share conventions. The extension is a
Manifest V3 extension in plain JavaScript without a build step, unless the Phase 0
investigation shows that a bundler is needed.

## CLI commands

| Command | Purpose |
| --- | --- |
| `keyguard install-bridge` | Register the native messaging host on Windows and print the extension setup steps. |
| `keyguard doctor` | Check the bridge, the extension connection, the dev.meta.ai login, and the store. |
| `keyguard status` | Show active, overdue, and pending keys, and the Cost Center alert state. |
| `keyguard create <label> [--ttl 12h] [--auto-rotate]` | Create a key and store it. |
| `keyguard list [--all]` | List stored keys; `--all` includes unmanaged keys found on dev.meta.ai. |
| `keyguard get / env / exec` | Deliver a key to a program (see above). |
| `keyguard rotate <label> [--grace 5m]` | Replace a key and delete the old one after the grace period. |
| `keyguard extend <label> --ttl 4h` | Push back a key's expiry, up to the configured maximum. |
| `keyguard revoke <label>` | Delete one key now. |
| `keyguard kill [--yes] [--reason ...]` | Delete every key, managed or not, leaving only a sentinel. |
| `keyguard sync` | Reconcile the store with the key list on dev.meta.ai. |
| `keyguard reap` | Delete expired keys and check Cost Center; intended for a timer. |
| `keyguard bridge` | The long-running native host process, launched by Chrome. |
| `keyguard cost-center on/off/check` | Turn the Cost Center check on or off, or show which alerts are revoking keys. |
| `keyguard config [set/unset]` | Show or change settings. |
| `keyguard log` | Show the audit log. |
| `keyguard uninstall-bridge` | Remove the registry entry and installed files. |

## Phases

0. **Investigation.** Record exactly how dev.meta.ai lists, creates, and deletes keys
   (network requests and DOM), including how the secret is returned and how logged-out and
   MFA states appear. Prove a native messaging round trip from Chrome on Windows to a
   process in WSL. Write the findings to `plans/RECON.md`.
1. **Extension alone.** Popup buttons for list, create, and delete through the adapter,
   with no CLI involved.
2. **Bridge and CLI basics.** `install-bridge`, `doctor`, `create`, `list`, `revoke`,
   `sync`, and the key store.
3. **TTL and rotation.** The reaper, extension alarms, overdue reporting, `rotate`,
   `extend`, and key delivery commands.
4. **Kill switch and Cost Center.** Sentinel handling, `kill`, the Cost Center alert fields
   and `alert status --json`, and the Keyguard check.
5. **Optional.** Per-key spend from the dev.meta.ai Usage page.

## Decisions

These were the open questions; each is followed by the answer given on 2026-10-25.

1. **Who uses the keys?** Is it only Muse Code and local scripts on this machine, or will
   keys also be deployed to servers, CI, or teammates' machines? A key that leaves this
   machine cannot be delivered by `exec`, and its usage is invisible to Cost Center.
   *Default: local use only.*
Anwser: local use only, Muse Code and Local Scripts
2. **How does Muse Code read its key?** If it reads `META_API_KEY` (or `MUSE_API_KEY`)
   from the environment, `keyguard exec` is enough; if it stores the key in its own config
   file, rotation must also update that file.
Anwser: If the user has a .env and stores the key, thats there problem. We can't safeguard where they dont' follow keygaurd practices
3. **One team or several?** Keys on dev.meta.ai belong to a team. Should Keyguard manage
   more than one team or account? *Default: one.*
Anwser: This is for a single person
4. **What happens to unmanaged keys?** When `kill` runs, should keys created by hand be
   deleted, left alone, or adopted into the store? *Default: left alone unless
   `--include-unmanaged` is passed, but always listed as a warning.*
Anwser: delete all keys.
5. **Should the kill switch be automatic?** Is it acceptable for a Cost Center limit to
   revoke keys with no confirmation, which will interrupt running work? *Default: yes at
   100% of the limit, notify only at lower levels.*
Anwser: Cost Center has notification thats are project specific or across all, I think we need what condition occurs when the notification hits it limit, since keys need names when entering in meta, I think you could have a field in cost center with some of matching syntax, eg. * for all keys, bootcamp-* for keys that match that pattern. or key1,key2 to match on specific keys. In cost center we'd have to checkbox on that feature. For our keyguard CLI we would want a flag if it should respect and check cost center for its ability to delete keys.
6. **How should secrets be protected at rest?** The default is a `0600` file, the same
   protection as `~/.aws/credentials`. Encrypting with a passphrase is safer but makes the
   unattended reaper and `exec` harder to use.
Anwser: For now plain.
7. **Internal requests or DOM automation?** Are you comfortable with the extension calling
   dev.meta.ai's undocumented internal endpoints, or should it only drive the visible UI?
- undocument internal enpoints, but if it doesn't work we really need to make sure the user know of failure.
8. **Who else will install this?** If teammates will use it, the extension needs a stable
   ID (a pinned key in the manifest, or a Chrome Web Store listing) so that the native
   host manifest can allow it.
Anwser: this is only for a single person, we aren't builing this for teams.
9. **Naming.** The notes use "Meta Cost Control", "Muse Cost Center", and "Meta Keyguard"
   / "Muse Keyguard". Which names should the tool and its documentation use?
Its Meta Cost Center. so this will be Meta Keyguard.
