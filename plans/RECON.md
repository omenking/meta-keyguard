# Recon: dev.meta.ai key management (2026-10-25)

This records what is known about the dashboard's internal API, where it came from, and what
is still unconfirmed. The extension's `adapter.js` is built on it, and when Meta changes the
dashboard this file and `adapter.js` are the two places to update.

## Source

The dashboard at https://dev.meta.ai/ is a Next.js application. Its JavaScript chunks are
served publicly from `/_next/static/immutable/chunks/`, and the module that implements the
API keys page's data layer (a React Query hook keyed `team-api-keys`) was found in one of
them. Nothing below required signing in, and no request was made to an authenticated
endpoint.

## Endpoints

| Purpose | Request | Notes |
| --- | --- | --- |
| Teams | `GET /api/portal/teams` | Returns `{ teams: [...] }`. The dashboard keeps the selected team in a `team_id` query parameter. |
| List keys | `GET /api/portal/teams/{teamId}/api-keys[?project_id=]` | Returns `api_keys` (or `data`), plus `can_create_api_key` and `api_key_creation_block_reason` (`age_restricted` or `payment_method_required`). |
| Create key | `POST /api/portal/teams/{teamId}/api-keys` | Body `{ "name": ..., "project_id": ... }`. Confirmed 2026-10-25: without `project_id` the request fails with 400 ("violated JSON schema constraint 'required' for the JSON field 'project_id'"). |
| Rename key | `PATCH /api/portal/api-keys/{apiKeyId}` | Body `{ "name": ... }`. Not used by Keyguard. |
| Delete key | `DELETE /api/portal/api-keys/{apiKeyId}` | |
| Current user | `GET /api/auth/me` | Not used by Keyguard. |

Every request carries only `Content-Type: application/json` and relies on the session cookie.
The dashboard's fetch wrapper turns a non-JSON error into "Could not reach the API" and reads
`error_code` and `action_url` from JSON errors, which is how a step-up ("confirm your
identity") prompt is most likely signalled. A 403 whose `detail` is
`payment_method_required` or `age_restricted` means key creation is blocked.

## Still unconfirmed

These can only be confirmed while signed in, so the adapter handles them defensively and
fails loudly if its assumptions are wrong:

1. **The create response shape** — *confirmed 2026-10-25*: `{ api_key_id, name, is_default,
   creation_time, creator_name, project_id, project_name, access_token }`, with the secret in
   the 48-character `access_token`. The adapter still also accepts the secret under `key`, `api_key`, `secret`, `secret_key`, `value`, `token`,
   `plaintext`, `plaintext_key`, `raw_key`, or `full_key`, at the top level or under
   `api_key` or `data`. If it finds none, or more than one candidate, it reports
   `secret_not_found`, deletes the key it just created, and includes the redacted shape of
   the response so the adapter can be corrected.
2. **The fields of a key in the list response.** The adapter reads `id` (or `api_key_id` /
   `key_id`), `name`, and `created_at` (or `created_time` / `creation_time`). A list item
   without an ID is reported as `endpoint_changed`.
3. **Where the project ID comes from.** Create requires one, but the projects endpoint is
   not in the public bundle. The extension uses, in order, the `project_id` setting, the
   `?project_id=` of an open dashboard tab, and a `projects` list or `default_project_id` on
   the team, and reports `project_unknown` or `multiple_projects` otherwise.
4. **How the logged-out and step-up states look.** A 401, a redirect to a login URL, or a
   non-JSON 200 is treated as `not_logged_in`; a 403 carrying `action_url` is treated as
   `reauth_required`.

`keyguard doctor --inspect` prints the redacted shape of a live list response (every string
replaced by its length), which is enough to correct items 2 and 4 without exposing a secret.
The first `keyguard create` confirms item 1 either way.

## Bridge

The native messaging path was verified from WSL on 2026-10-25 by launching the installed
`keyguard-host.bat` through `cmd.exe`, as Chrome does, and exchanging framed messages: a
200 KB reply and non-ASCII text arrived intact through `wsl.exe`. Two details came out of
that test. First, `wsl.exe -d "<distro>"` treats the quotes as part of the name, so the
launcher passes the distro name bare, although quoted `--exec` arguments behave normally.
Second, Unix socket paths are limited to 108 bytes, so a deep data directory falls back to
a socket in `$XDG_RUNTIME_DIR`.

What has not been verified is the extension running inside Chrome itself, since that
requires loading it in the user's browser.
