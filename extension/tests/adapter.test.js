import assert from 'node:assert/strict';
import { test } from 'node:test';
import { AdapterError, checkResponse, chooseProject, createKey, deleteKeySafely, extractCreated, makeApi, normalizeKeyList, normalizeTeams, shapeOf } from '../adapter.js';

const json = (status, body, extra = {}) => ({ status, ok: status >= 200 && status < 300, url: 'https://dev.meta.ai/api/portal/x',
  redirected: false, contentType: 'application/json', text: JSON.stringify(body), ...extra });

/** A pretend dev.meta.ai that enforces the last-key rule, answering in the dashboard's shapes. */
function fakeMeta({ createBody } = {}) {
  const keys = [{ id: '1', name: 'existing' }];
  let next = 2;
  const calls = [];
  const request = async (path, init) => {
    calls.push(`${init.method} ${path}`);
    if (path === '/api/portal/teams') return json(200, { teams: [{ id: 't1', name: 'Me' }] });
    if (path === '/api/portal/teams/t1/api-keys' && init.method === 'GET') return json(200, { api_keys: keys, can_create_api_key: true });
    if (path === '/api/portal/teams/t1/api-keys' && init.method === 'POST') {
      const id = String(next++);
      if (!init.body.project_id) return json(400, { detail: "violated JSON schema constraint 'required' for the JSON field 'project_id'" });
      keys.push({ id, name: init.body.name, project_id: init.body.project_id });
      return json(200, createBody ? createBody(id, init.body.name) : { id, name: init.body.name, key: `sk-meta-${id}-abcdefghijklmnop` });
    }
    const match = path.match(/^\/api\/portal\/api-keys\/(.+)$/);
    if (match && init.method === 'DELETE') {
      const index = keys.findIndex(k => k.id === match[1]);
      if (index < 0) return json(404, { detail: 'not found' });
      if (keys.length === 1) return json(400, { detail: 'cannot delete last key' });
      keys.splice(index, 1);
      return { status: 204, ok: true, url: '', contentType: '', text: '' };
    }
    return json(404, {});
  };
  return { keys, calls, api: makeApi(request) };
}

test('lists teams and keys in either response layout', () => {
  assert.deepEqual(normalizeTeams({ teams: [{ id: 5, name: 'A' }] }), [{ id: '5', name: 'A', projects: [], default_project_id: null }]);
  assert.equal(normalizeKeyList({ data: [{ id: 'k', name: 'n' }] }).keys[0].id, 'k');
  assert.equal(normalizeKeyList({ api_keys: [], can_create_api_key: false }).can_create, false);
  assert.throws(() => normalizeKeyList({ keys: [] }), e => e.code === 'endpoint_changed');
  assert.throws(() => normalizeKeyList({ api_keys: [{ name: 'no id' }] }), e => e.code === 'endpoint_changed');
});

test('classifies failures so the user is told what went wrong', () => {
  const code = response => { try { checkResponse(response, 'x'); } catch (e) { return e.code; } };
  assert.equal(code(json(401, {})), 'not_logged_in');
  assert.equal(code({ status: 200, ok: true, redirected: true, url: 'https://dev.meta.ai/login', contentType: 'text/html', text: '<html>' }), 'not_logged_in');
  assert.equal(code(json(403, { detail: 'payment_method_required' })), 'create_blocked');
  assert.equal(code(json(403, { error_code: 'x', action_url: 'https://example' })), 'reauth_required');
  assert.equal(code(json(403, { detail: 'nope' })), 'forbidden');
  assert.equal(code(json(404, {})), 'not_found');
  assert.equal(code(json(429, {})), 'rate_limited');
  assert.equal(code(json(502, {})), 'meta_unavailable');
  assert.equal(code({ status: 200, ok: true, contentType: 'text/html', text: '<html>', url: 'https://dev.meta.ai/api/portal/x' }), 'endpoint_changed');
  assert.equal(code({ networkError: 'TypeError: Failed to fetch' }), 'network');
});

test('finds the secret in the likely create response layouts', () => {
  const secret = 'sk-meta-0123456789abcdef';
  for (const body of [{ id: 'a', key: secret }, { api_key: { id: 'a', secret } }, { data: { id: 'a', value: secret } }, { id: 'a', api_key: secret }]) {
    assert.deepEqual({ ...extractCreated(body), name: undefined }, { id: 'a', name: undefined, secret }, JSON.stringify(body));
  }
});

test('reads the create response dev.meta.ai actually returns', () => {
  const body = { api_key_id: '1492939029370457', name: 'kgtest-kg20261025170941', is_default: false, creation_time: 1792946981,
    creator_name: 'Andrew Brown', project_id: '1573755430880189', project_name: 'Default Project', access_token: 'x'.repeat(48) };
  assert.deepEqual(extractCreated(body), { id: '1492939029370457', name: 'kgtest-kg20261025170941', secret: 'x'.repeat(48) });
});

test('refuses to guess when the secret is missing or ambiguous', () => {
  assert.throws(() => extractCreated({ id: 'a', name: 'x' }), e => e.code === 'secret_not_found' && e.detail.id === 'a');
  assert.throws(() => extractCreated({ id: 'a', key: 'sk-aaaaaaaaaaaaaaaaaa', secret: 'sk-bbbbbbbbbbbbbbbbbbbb' }), e => e.code === 'secret_not_found');
});

test('shapes never contain string values', () => {
  const shape = JSON.stringify(shapeOf({ key: 'sk-secret-value', nested: [{ token: 'abc' }], n: 3 }));
  assert.ok(!shape.includes('sk-secret') && !shape.includes('abc'));
  assert.match(shape, /<string 15>/);
});

test('createKey deletes the new key again when its secret cannot be found', async () => {
  const meta = fakeMeta({ createBody: id => ({ id, name: 'x' }) });
  await assert.rejects(createKey(meta.api, 't1', 'dev-kg1', 'p1'), e => e.code === 'secret_not_found' && e.detail.cleaned_up === true);
  assert.deepEqual(meta.keys.map(k => k.id), ['1']);
});

test('createKey finds the leftover key by name when the response has no ID', async () => {
  const meta = fakeMeta({ createBody: () => ({ ok: true }) });
  await assert.rejects(createKey(meta.api, 't1', 'dev-kg2', 'p1'), e => e.code === 'secret_not_found' && e.detail.cleaned_up === true);
  assert.deepEqual(meta.keys.map(k => k.name), ['existing']);
});

test('deleting the last key creates a sentinel first and never keeps its secret', async () => {
  const meta = fakeMeta();
  let sentinel;
  const result = await deleteKeySafely(meta.api, 't1', '1', s => { sentinel = s; }, 'p1');
  assert.equal(result.deleted, true);
  assert.deepEqual(meta.keys.map(k => k.name), ['keyguard-sentinel']);
  assert.equal(sentinel.secret, null);
  assert.deepEqual(await deleteKeySafely(meta.api, 't1', '1'), { deleted: false, already_gone: true });
});

test('keys are created in the chosen project', async () => {
  const meta = fakeMeta();
  await assert.rejects(createKey(meta.api, 't1', 'no-project'), e => e.code === 'endpoint_changed' && /project_id/.test(e.message));
  const created = await createKey(meta.api, 't1', 'dev', 'p9');
  assert.equal(meta.keys.find(k => k.id === created.id).project_id, 'p9');
});

test('project choice: explicit, then the tab, then the team default or only project', () => {
  const team = normalizeTeams({ teams: [{ id: 't', projects: [{ id: 'a' }, { id: 'b' }] }] })[0];
  assert.equal(chooseProject(team, 'x', 'y'), 'x');
  assert.equal(chooseProject(team, null, 'y'), 'y');
  assert.throws(() => chooseProject(team), e => e.code === 'multiple_projects' && e.detail.projects.length === 2);
  assert.equal(chooseProject(normalizeTeams({ teams: [{ id: 't', default_project_id: 7 }] })[0]), '7');
  assert.equal(chooseProject(normalizeTeams({ teams: [{ id: 't', projects: [{ id: 'only' }] }] })[0]), 'only');
  assert.throws(() => chooseProject(normalizeTeams({ teams: [{ id: 't' }] })[0]), e => e.code === 'project_unknown');
});

test('team selection asks the user to choose when there are several', async () => {
  const api = makeApi(async () => json(200, { teams: [{ id: 'a' }, { id: 'b' }] }));
  await assert.rejects(api.team(), e => e instanceof AdapterError && e.code === 'multiple_teams');
  assert.equal((await api.team('b')).id, 'b');
});
