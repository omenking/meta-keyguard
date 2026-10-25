// Meta Keyguard service worker. It performs key operations for the CLI over native messaging,
// and deletes expired keys on its own when the CLI cannot (for example, while WSL is stopped).
import { AdapterError, SENTINEL_NAME, chooseProject, createKey, deleteKeySafely, makeApi, shapeOf } from './adapter.js';

const HOST = 'com.metakeyguard.bridge';
const VERSION = chrome.runtime.getManifest().version;
const ORIGIN = 'https://dev.meta.ai';
// While the bridge is connected the CLI's reaper handles expiry; the extension steps in when the
// bridge is gone, or when a deletion is this far overdue (the CLI is evidently failing).
const OVERDUE_GRACE_MS = 5 * 60 * 1000;
const NOTIFY_EVERY_MS = 10 * 60 * 1000;

let port = null;
// Event IDs already sent on the current connection; the bridge acknowledges each one.
let sent = new Set();

// Storage ------------------------------------------------------------------------------------

async function get(key, fallback) {
  const value = (await chrome.storage.local.get(key))[key];
  return value === undefined ? fallback : value;
}
const put = (key, value) => chrome.storage.local.set({ [key]: value });

async function updateStatus(change) {
  const status = { ...(await get('status', {})), ...change };
  await put('status', status);
  await refreshBadge(status);
  return status;
}

async function refreshBadge(status) {
  status ??= await get('status', {});
  const schedule = await get('schedule', []);
  const overdue = schedule.filter(k => Date.parse(k.delete_at) <= Date.now()).length;
  const failing = status.lastError && (!status.lastOk || status.lastError.at > status.lastOk);
  const text = failing || overdue ? '!' : status.bridge === 'connected' ? '' : '·';
  await chrome.action.setBadgeText({ text });
  await chrome.action.setBadgeBackgroundColor({ color: failing || overdue ? '#b42318' : '#6b7280' });
  const title = failing ? `Meta Keyguard: ${status.lastError.message}` : overdue ? `Meta Keyguard: ${overdue} key(s) overdue for deletion`
    : status.bridge === 'connected' ? 'Meta Keyguard: connected' : 'Meta Keyguard: CLI bridge not connected';
  await chrome.action.setTitle({ title });
}

/** Failures are always made visible: a red badge, the popup, the CLI, and a desktop notification. */
async function reportFailure(op, error, metaId) {
  const failure = { at: new Date().toISOString(), op, code: error.code || 'unknown', message: error.message, meta_id: metaId ?? null };
  await updateStatus({ lastError: failure });
  const notified = await get('notified', {});
  const key = `${op}:${failure.code}`;
  if (!notified[key] || Date.now() - notified[key] > NOTIFY_EVERY_MS) {
    notified[key] = Date.now();
    await put('notified', notified);
    chrome.notifications.create({ type: 'basic', iconUrl: 'icon-128.png', title: `Meta Keyguard: ${op} failed`, message: failure.message, priority: 2 });
  }
  return failure;
}

// Running requests inside a dev.meta.ai tab ----------------------------------------------------

// Runs in the page's isolated world, so the request carries the dev.meta.ai session cookie and
// the page's own scripts cannot see it.
async function pageFetch(path, method, body) {
  try {
    const response = await fetch(path, {
      method, credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
      body: body == null ? undefined : JSON.stringify(body),
    });
    return { status: response.status, ok: response.ok, url: response.url, redirected: response.redirected,
      contentType: response.headers.get('content-type') || '', text: await response.text() };
  } catch (error) {
    return { networkError: String(error) };
  }
}

function waitForLoad(tabId, timeoutMs = 30000) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => { chrome.tabs.onUpdated.removeListener(listener); reject(new AdapterError('tab_failed', 'dev.meta.ai did not finish loading.')); }, timeoutMs);
    function listener(id, info) {
      if (id === tabId && info.status === 'complete') { clearTimeout(timer); chrome.tabs.onUpdated.removeListener(listener); resolve(); }
    }
    chrome.tabs.onUpdated.addListener(listener);
    chrome.tabs.get(tabId).then(tab => { if (tab.status === 'complete') listener(tabId, { status: 'complete' }); }).catch(() => {});
  });
}

/** The project the dashboard tab is showing, from its `project_id` query parameter. */
async function tabProject(tabId) {
  try {
    return new URL((await chrome.tabs.get(tabId)).url).searchParams.get('project_id');
  } catch {
    return null;
  }
}

/** Resolve and remember the project new keys go into; the extension needs it for sentinels too. */
async function resolveProject(team, preferredId, tabId) {
  const projectId = chooseProject(team, preferredId || (await get('projectId', null)), await tabProject(tabId));
  await put('projectId', projectId);
  return projectId;
}

/** Run `fn(api, tabId)` against a dev.meta.ai tab, opening a background tab if none is usable. */
async function withMeta(fn) {
  const tabs = (await chrome.tabs.query({ url: `${ORIGIN}/*` })).filter(t => !t.discarded);
  let tab = tabs.find(t => t.status === 'complete');
  let opened = false;
  if (!tab) {
    tab = await chrome.tabs.create({ url: `${ORIGIN}/api-keys`, active: false });
    opened = true;
    await waitForLoad(tab.id);
  }
  const request = async (path, init) => {
    let results;
    try {
      results = await chrome.scripting.executeScript({ target: { tabId: tab.id }, func: pageFetch, args: [path, init.method, init.body ?? null] });
    } catch (error) {
      throw new AdapterError('tab_failed', `Could not run in the dev.meta.ai tab: ${error.message}`);
    }
    return results[0]?.result ?? { networkError: 'no result from the tab' };
  };
  try {
    return await fn(makeApi(request), tab.id);
  } finally {
    if (opened) chrome.tabs.remove(tab.id).catch(() => {});
  }
}

// Operations requested by the CLI ----------------------------------------------------------------

async function queueEvent(event) {
  const events = await get('events', []);
  events.push({ ...event, event_id: crypto.randomUUID(), at: new Date().toISOString() });
  await put('events', events);
  flushEvents();
}

async function flushEvents() {
  if (!port) return;
  for (const event of await get('events', [])) {
    if (sent.has(event.event_id)) continue;
    sent.add(event.event_id);
    port.postMessage({ type: 'event', event });
  }
}

const OPS = {
  async ping() { return { version: VERSION }; },

  async whoami({ team_id }) {
    return withMeta(async api => {
      const team = await api.team(team_id);
      await put('teamId', team.id);
      const list = await api.listKeys(team.id);
      return { team, key_count: list.keys.length, can_create: list.can_create, block_reason: list.block_reason };
    });
  },

  async list({ team_id }) {
    return withMeta(async api => {
      const team = await api.team(team_id);
      await put('teamId', team.id);
      return { team, ...(await api.listKeys(team.id)) };
    });
  },

  async create({ team_id, project_id, name, discard_secret }) {
    if (typeof name !== 'string' || !name) throw new AdapterError('bad_request', 'A key name is required.');
    return withMeta(async (api, tabId) => {
      const team = await api.team(team_id);
      const created = await createKey(api, team.id, name, await resolveProject(team, project_id, tabId));
      // The sentinel's secret is dropped here, so it never leaves the browser.
      if (discard_secret) created.secret = null;
      return { id: created.id, name: created.name, secret: created.secret, team_id: team.id };
    });
  },

  async delete({ team_id, id }) {
    return withMeta(async api => {
      const team = await api.team(team_id);
      const result = await api.deleteKey(String(id));
      await dropFromSchedule(String(id));
      return { ...result, team_id: team.id };
    });
  },

  async schedule({ keys }) {
    if (!Array.isArray(keys)) throw new AdapterError('bad_request', 'schedule needs a list of keys.');
    await put('schedule', keys.map(k => ({ id: String(k.id), name: k.name, label: k.label, delete_at: k.delete_at })));
    await setAlarms();
    await refreshBadge();
    return { scheduled: keys.length };
  },

  async inspect({ team_id, project_id }) {
    // For diagnosing adapter breakage: response shapes with every string replaced by its length.
    return withMeta(async (api, tabId) => {
      const team = await api.team(team_id);
      const list = await api.listKeys(team.id);
      const teams = await api.teamsRaw();
      let project;
      try { project = { id: chooseProject(team, project_id || (await get('projectId', null)), await tabProject(tabId)) }; } catch (error) { project = { error: error.code, detail: error.detail }; }
      return { team_count: normalizeTeamsCount(teams), first_team_fields: shapeOf((teams?.teams ?? teams)?.[0] ?? null), key_count: list.keys.length, first_key_fields: shapeOf(list.keys[0] ?? null),
        can_create: list.can_create, block_reason: list.block_reason, project };
    });
  },
};

const normalizeTeamsCount = body => (Array.isArray(body?.teams) ? body.teams : Array.isArray(body) ? body : []).length;

async function handleRequest(message) {
  const { request_id, op, params = {} } = message;
  const handler = OPS[op];
  try {
    if (!handler) throw new AdapterError('bad_request', `Unknown operation ${op}.`);
    const result = await handler(params);
    if (op !== 'ping' && op !== 'schedule') await updateStatus({ lastOk: new Date().toISOString() });
    port?.postMessage({ request_id, ok: true, result });
  } catch (error) {
    const code = error instanceof AdapterError ? error.code : 'extension_error';
    await reportFailure(op, { code, message: error.message }, params.id);
    port?.postMessage({ request_id, ok: false, error: { code, message: error.message, detail: error.detail ?? null } });
  }
}

// The bridge -------------------------------------------------------------------------------------

function connect() {
  if (port) return;
  try {
    port = chrome.runtime.connectNative(HOST);
    sent = new Set();
  } catch (error) {
    updateStatus({ bridge: 'disconnected', bridgeError: error.message });
    return;
  }
  port.onMessage.addListener(message => {
    if (message.op) handleRequest(message);
    else if (message.type === 'event_ack') ackEvent(message.event_id);
  });
  port.onDisconnect.addListener(() => {
    const reason = chrome.runtime.lastError?.message || 'The bridge exited.';
    port = null;
    updateStatus({ bridge: 'disconnected', bridgeError: reason });
  });
  port.postMessage({ type: 'hello', version: VERSION });
  updateStatus({ bridge: 'connected', bridgeError: null, connectedAt: new Date().toISOString() });
  flushEvents();
}

async function ackEvent(eventId) {
  await put('events', (await get('events', [])).filter(e => e.event_id !== eventId));
}

// Expiry enforcement -----------------------------------------------------------------------------

async function dropFromSchedule(id) {
  await put('schedule', (await get('schedule', [])).filter(k => k.id !== id));
  chrome.alarms.clear(`expire:${id}`);
}

async function setAlarms() {
  const schedule = await get('schedule', []);
  for (const alarm of await chrome.alarms.getAll()) {
    if (alarm.name.startsWith('expire:') && !schedule.some(k => `expire:${k.id}` === alarm.name)) chrome.alarms.clear(alarm.name);
  }
  for (const key of schedule) {
    chrome.alarms.create(`expire:${key.id}`, { when: Math.max(Date.now() + 1000, Date.parse(key.delete_at) + OVERDUE_GRACE_MS) });
  }
}

let enforcing = false;

/** Delete keys that are past due, if the CLI is not there to do it or has failed to. */
async function enforceExpiry() {
  if (enforcing) return;
  enforcing = true;
  try {
    const schedule = await get('schedule', []);
    const due = schedule.filter(k => {
      const late = Date.now() - Date.parse(k.delete_at);
      return late >= 0 && (!port || late >= OVERDUE_GRACE_MS);
    });
    if (!due.length) return;
    await withMeta(async (api, tabId) => {
      const team = await api.team((await get('teamId', null)) || undefined);
      let projectId = null;
      try { projectId = await resolveProject(team, null, tabId); } catch { /* only needed if a sentinel must be made */ }
      for (const key of due) {
        try {
          const result = await deleteKeySafely(api, team.id, key.id, sentinel => queueEvent({ kind: 'sentinel-created', meta_id: sentinel.id, name: sentinel.name }), projectId);
          await dropFromSchedule(key.id);
          await queueEvent({ kind: 'deleted', meta_id: key.id, reason: result.already_gone ? 'expired (already gone)' : 'expired (deleted by the extension)' });
        } catch (error) {
          await reportFailure('expire', error, key.id);
          await queueEvent({ kind: 'failure', op: 'expire', code: error.code, message: error.message, meta_id: key.id });
        }
      }
    });
  } catch (error) {
    await reportFailure('expire', error);
  } finally {
    enforcing = false;
    refreshBadge();
  }
}

// Lifecycle --------------------------------------------------------------------------------------

chrome.runtime.onInstalled.addListener(() => { chrome.alarms.create('tick', { periodInMinutes: 1 }); connect(); enforceExpiry(); });
chrome.runtime.onStartup.addListener(() => { chrome.alarms.create('tick', { periodInMinutes: 1 }); connect(); enforceExpiry(); });
chrome.alarms.onAlarm.addListener(alarm => {
  connect();
  if (alarm.name === 'tick' || alarm.name.startsWith('expire:')) enforceExpiry();
});

// The popup asks for status and can trigger a reconnect or a dev.meta.ai check.
chrome.runtime.onMessage.addListener((message, _sender, reply) => {
  (async () => {
    if (message.popup === 'reconnect') { port?.disconnect(); port = null; connect(); }
    if (message.popup === 'check') {
      try {
        const who = await OPS.whoami({ team_id: (await get('teamId', null)) || undefined });
        await updateStatus({ lastOk: new Date().toISOString(), meta: { team: who.team, key_count: who.key_count, checked_at: new Date().toISOString() } });
      } catch (error) {
        await reportFailure('check', error);
      }
    }
    if (message.popup === 'clear-error') await updateStatus({ lastError: null });
    reply({ status: await get('status', {}), schedule: await get('schedule', []), pendingEvents: (await get('events', [])).length });
  })();
  return true;
});

connect();
