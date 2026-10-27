// The extension is deliberately disabled (see the warning at the top of background.js), so the
// popup only shows its warning and never talks to the service worker, which no longer runs.
throw new Error('Meta Keyguard is disabled; see background.js.');

const $ = id => document.getElementById(id);

function remaining(when) {
  const ms = Date.parse(when) - Date.now();
  const minutes = Math.round(Math.abs(ms) / 60000);
  const text = minutes >= 60 ? `${Math.floor(minutes / 60)}h ${minutes % 60}m` : `${minutes}m`;
  return ms <= 0 ? `overdue ${text}` : `in ${text}`;
}

function render({ status, schedule, pendingEvents }) {
  const connected = status.bridge === 'connected';
  $('bridge').textContent = connected ? 'Connected' : `Not connected${status.bridgeError ? `: ${status.bridgeError}` : ''}`;
  $('bridge').className = connected ? 'ok' : 'bad';
  if (status.meta) {
    $('meta').textContent = `Signed in · team ${status.meta.team.name || status.meta.team.id} · ${status.meta.key_count} key(s)`;
    $('meta').className = 'ok';
  }
  const error = status.lastError && (!status.lastOk || status.lastError.at > status.lastOk) ? status.lastError : null;
  $('error').hidden = !error;
  if (error) {
    $('error-title').textContent = `${error.op} failed (${error.code})`;
    $('error-message').textContent = error.message;
    if (error.op === 'check') { $('meta').textContent = error.message; $('meta').className = 'bad'; }
  }
  const list = $('schedule');
  list.replaceChildren();
  for (const key of schedule) {
    const item = document.createElement('li');
    const name = document.createElement('span');
    name.textContent = key.label || key.name;
    name.title = key.name;
    const when = document.createElement('span');
    when.textContent = remaining(key.delete_at);
    if (Date.parse(key.delete_at) <= Date.now()) when.className = 'bad';
    item.append(name, when);
    list.append(item);
  }
  if (!schedule.length) list.innerHTML = '<li class="muted">No keys scheduled.</li>';
  $('pending').hidden = !pendingEvents;
  $('pending').textContent = `${pendingEvents} change(s) waiting to reach the CLI.`;
}

const ask = popup => chrome.runtime.sendMessage({ popup }).then(render);
$('check').addEventListener('click', () => { $('meta').textContent = 'Checking…'; $('meta').className = ''; ask('check'); });
$('reconnect').addEventListener('click', () => ask('reconnect'));
$('clear-error').addEventListener('click', () => ask('clear-error'));
ask('status');
