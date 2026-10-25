// Everything that depends on dev.meta.ai's undocumented internal API lives in this file, so
// that a change on Meta's side is fixed here and nowhere else.
//
// The endpoints below come from the dashboard's own JavaScript bundle (see plans/RECON.md):
//   GET    /api/portal/teams                      -> { teams: [...] }
//   GET    /api/portal/teams/{teamId}/api-keys    -> { api_keys | data: [...], can_create_api_key, api_key_creation_block_reason }
//   POST   /api/portal/teams/{teamId}/api-keys    (create; body { name, project_id }; response shape inferred)
//   DELETE /api/portal/api-keys/{apiKeyId}
// The dashboard sends only `Content-Type: application/json` and relies on the session cookie,
// so requests are made from inside a dev.meta.ai tab.

export const SENTINEL_NAME = 'keyguard-sentinel';

export class AdapterError extends Error {
  constructor(code, message, detail) {
    super(message);
    this.code = code;
    this.detail = detail;
  }
}

/** A copy of a JSON value with every string replaced by its length, safe to show or share. */
export function shapeOf(value, depth = 0) {
  if (depth > 6) return '…';
  if (Array.isArray(value)) return value.length ? [shapeOf(value[0], depth + 1), `(${value.length} items)`] : [];
  if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([k, v]) => [k, shapeOf(v, depth + 1)]));
  if (typeof value === 'string') return `<string ${value.length}>`;
  return value;
}

function parseBody(response) {
  if (!response.text) return null;
  try { return JSON.parse(response.text); } catch { return undefined; }
}

/** Turn a raw response into its JSON body, or throw an AdapterError the user can act on. */
export function checkResponse(response, what) {
  if (response.networkError) throw new AdapterError('network', `${what}: ${response.networkError}`);
  const body = parseBody(response);
  const isJson = (response.contentType || '').includes('json') && body !== undefined;
  const loginPage = /\/(login|auth)\b/.test(response.url || '');
  if (response.ok && isJson) return body;
  if (response.ok && response.status === 204) return null;
  if (response.status === 401 || loginPage || (response.ok && !isJson && response.redirected)) {
    throw new AdapterError('not_logged_in', 'You are not signed in to dev.meta.ai in this Chrome profile.');
  }
  const detail = body && typeof body === 'object' ? body : undefined;
  const reason = detail?.detail ?? detail?.error_code;
  if (response.status === 403) {
    if (reason === 'payment_method_required' || reason === 'age_restricted') {
      throw new AdapterError('create_blocked', `dev.meta.ai blocks key creation: ${reason}.`, { reason });
    }
    if (detail?.action_url || /reauth|confirm|identity|step.?up/i.test(String(reason ?? ''))) {
      throw new AdapterError('reauth_required', 'dev.meta.ai asks you to confirm your identity before managing keys.',
        { error_code: detail?.error_code, action_url: detail?.action_url });
    }
    throw new AdapterError('forbidden', `${what} was refused (403${reason ? `: ${reason}` : ''}).`, shapeOf(detail));
  }
  if (response.status === 404) throw new AdapterError('not_found', `${what}: not found (404).`, shapeOf(detail));
  if (response.status === 429) throw new AdapterError('rate_limited', `${what}: rate limited.`);
  if (response.status >= 500) throw new AdapterError('meta_unavailable', `${what}: dev.meta.ai returned ${response.status}.`);
  if (response.ok) {
    throw new AdapterError('endpoint_changed', `${what}: expected JSON but got ${response.contentType || 'no content type'}.`);
  }
  throw new AdapterError('endpoint_changed', `${what}: unexpected status ${response.status}${reason ? ` (${reason})` : ''}.`,
    shapeOf(detail));
}

const idOf = item => item?.id ?? item?.api_key_id ?? item?.key_id ?? item?.team_id;

export function normalizeTeams(body) {
  const teams = Array.isArray(body?.teams) ? body.teams : Array.isArray(body) ? body : null;
  if (!teams) throw new AdapterError('endpoint_changed', 'The teams response has no teams list.', shapeOf(body));
  return teams.map(t => {
    const projects = (Array.isArray(t?.projects) ? t.projects : []).map(p => ({ id: String(p?.id ?? p?.project_id ?? ''), name: p?.name ?? '' })).filter(p => p.id);
    const fallback = t?.default_project_id ?? t?.default_project?.id ?? t?.project_id ?? null;
    return { id: String(idOf(t) ?? ''), name: t?.name ?? t?.display_name ?? t?.team_name ?? '', projects,
      default_project_id: fallback == null ? null : String(fallback) };
  }).filter(t => t.id);
}

/**
 * dev.meta.ai creates keys inside a project. Prefer an explicit choice, then the project the
 * open dashboard tab is on, then the team's default or only project.
 */
export function chooseProject(team, preferredId, fromTabUrl) {
  if (preferredId) return String(preferredId);
  if (fromTabUrl) return String(fromTabUrl);
  if (team.default_project_id) return team.default_project_id;
  if (team.projects?.length === 1) return team.projects[0].id;
  if (team.projects?.length > 1) {
    throw new AdapterError('multiple_projects', 'This team has several projects.', { projects: team.projects });
  }
  throw new AdapterError('project_unknown', 'Keyguard could not work out which dev.meta.ai project to create the key in.', { team_fields: Object.keys(team) });
}

export function normalizeKeyList(body) {
  const items = Array.isArray(body?.api_keys) ? body.api_keys : Array.isArray(body?.data) ? body.data : null;
  if (!items) throw new AdapterError('endpoint_changed', 'The API keys response has no key list.', shapeOf(body));
  const keys = items.map(item => ({
    id: idOf(item) == null ? null : String(idOf(item)),
    name: item?.name ?? item?.display_name ?? '',
    created_at: item?.created_at ?? item?.created_time ?? item?.creation_time ?? null,
  }));
  if (keys.some(k => !k.id)) throw new AdapterError('endpoint_changed', 'An API key in the list has no ID.', shapeOf(body));
  return {
    keys,
    can_create: typeof body.can_create_api_key === 'boolean' ? body.can_create_api_key : null,
    block_reason: body.api_key_creation_block_reason ?? null,
  };
}

// `access_token` is what dev.meta.ai actually returns (confirmed 2026-10-25); the rest are fallbacks.
const SECRET_FIELDS = ['access_token', 'key', 'api_key', 'secret', 'secret_key', 'value', 'token', 'plaintext', 'plaintext_key', 'raw_key', 'full_key'];
const looksSecret = value => typeof value === 'string' && value.length >= 16 && !/\s/.test(value);

/**
 * Find the new key's ID and secret in a create response. The exact shape has not been seen
 * yet, so this accepts the likely layouts and refuses anything ambiguous rather than guess.
 */
export function extractCreated(body) {
  const containers = [body, body?.api_key, body?.data, body?.data?.api_key].filter(c => c && typeof c === 'object');
  const id = containers.map(idOf).find(v => v != null);
  const candidates = new Set();
  for (const container of containers) {
    for (const field of SECRET_FIELDS) {
      const value = container[field];
      if (looksSecret(value) && String(value) !== String(id)) candidates.add(value);
    }
  }
  const name = containers.map(c => c.name).find(v => typeof v === 'string') ?? null;
  if (candidates.size !== 1) {
    throw new AdapterError('secret_not_found',
      candidates.size ? 'The create response holds more than one value that could be the secret.' : 'The create response holds no secret.',
      { id: id == null ? null : String(id), shape: shapeOf(body) });
  }
  if (id == null) throw new AdapterError('endpoint_changed', 'The create response has no key ID.', { shape: shapeOf(body) });
  return { id: String(id), name, secret: [...candidates][0] };
}

/** The dev.meta.ai operations Keyguard needs, built on a `request(path, init)` that runs in a tab. */
export function makeApi(request) {
  const json = async (path, init, what) => checkResponse(await request(path, init), what);
  return {
    async teamsRaw() { return json('/api/portal/teams', { method: 'GET' }, 'Listing teams'); },
    async teams() { return normalizeTeams(await this.teamsRaw()); },
    async team(preferredId) {
      const teams = await this.teams();
      if (preferredId) {
        const team = teams.find(t => t.id === String(preferredId));
        if (!team) throw new AdapterError('forbidden', `Team ${preferredId} is not one of your teams.`, { teams });
        return team;
      }
      if (teams.length === 1) return teams[0];
      if (!teams.length) throw new AdapterError('forbidden', 'This account has no team with Model API access.');
      throw new AdapterError('multiple_teams', 'This account belongs to several teams.', { teams });
    },
    async listKeys(teamId) {
      return normalizeKeyList(await json(`/api/portal/teams/${encodeURIComponent(teamId)}/api-keys`, { method: 'GET' }, 'Listing API keys'));
    },
    async createKeyRaw(teamId, name, projectId) {
      return json(`/api/portal/teams/${encodeURIComponent(teamId)}/api-keys`, { method: 'POST', body: { name, project_id: projectId } }, 'Creating an API key');
    },
    async deleteKey(id) {
      try {
        await json(`/api/portal/api-keys/${encodeURIComponent(id)}`, { method: 'DELETE' }, 'Deleting an API key');
        return { deleted: true };
      } catch (error) {
        // Already gone is what we wanted.
        if (error.code === 'not_found') return { deleted: false, already_gone: true };
        throw error;
      }
    },
  };
}

/**
 * Create a key and return its ID and secret. If the secret cannot be found, the key that was
 * just created is deleted again, so a failure never leaves behind a key nobody can use or track.
 */
export async function createKey(api, teamId, name, projectId) {
  const body = await api.createKeyRaw(teamId, name, projectId);
  try {
    const created = extractCreated(body);
    return { ...created, name: created.name ?? name };
  } catch (error) {
    let id = error.detail?.id;
    try {
      if (!id) id = (await api.listKeys(teamId)).keys.find(k => k.name === name)?.id;
      if (id) await api.deleteKey(id);
      error.detail = { ...error.detail, cleaned_up: Boolean(id) };
    } catch (cleanup) {
      error.detail = { ...error.detail, cleaned_up: false, cleanup_error: cleanup.message };
    }
    if (!error.detail.cleaned_up) error.message += ` A key named ${name} may exist on dev.meta.ai; delete it by hand.`;
    throw error;
  }
}

/** Delete a key, first creating a sentinel if dev.meta.ai would refuse to delete its last key. */
export async function deleteKeySafely(api, teamId, id, onSentinel, projectId) {
  const { keys } = await api.listKeys(teamId);
  if (!keys.some(k => k.id === id)) return { deleted: false, already_gone: true };
  if (keys.length === 1) {
    const sentinel = await createKey(api, teamId, SENTINEL_NAME, projectId);
    sentinel.secret = null;
    await onSentinel?.(sentinel);
  }
  return api.deleteKey(id);
}
