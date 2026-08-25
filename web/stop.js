/* RideWatch stop page: header, live arrivals, track record, push alerts. */

const UPCOMING_MS = 15000;

const stopId = decodeURIComponent(
  location.pathname.replace(/\/+$/, '').split('/').pop() || ''
);

async function fetchJSON(url, opts) {
  const res = await fetch(url, opts);
  if (!res.ok) throw new Error(url + ': HTTP ' + res.status);
  return res.json();
}

// The API's JSON field casing is not frozen yet; probe the plausible spellings.
function pick(obj, ...names) {
  if (!obj) return null;
  for (const n of names) {
    const v = obj[n];
    if (v !== undefined && v !== null && v !== '') return v;
  }
  return null;
}

function pickArray(obj, ...names) {
  if (Array.isArray(obj)) return obj;
  if (!obj) return [];
  for (const n of names) if (Array.isArray(obj[n])) return obj[n];
  return [];
}

function num(v) {
  const n = Number(v);
  return v === null || v === undefined || !Number.isFinite(n) ? null : n;
}

// Go's zero time marshals as year 1; treat anything pre-epoch as "no value".
function parseTime(v) {
  if (!v || typeof v !== 'string') return null;
  const d = new Date(v);
  return isNaN(d) || d.getFullYear() < 1971 ? null : d;
}

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function clockLabel(d) {
  return d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
}

// GTFS scheduled_secs may exceed 24h ("25:30" = 1:30 AM the next civil day);
// fold into a 12-hour clock label.
function gtfsClock(secs) {
  const h24 = Math.floor(secs / 3600) % 24;
  const m = Math.floor((secs % 3600) / 60);
  const h12 = h24 % 12 || 12;
  return h12 + ':' + String(m).padStart(2, '0') + ' ' + (h24 < 12 ? 'AM' : 'PM');
}

function fmtDelayMin(secs) {
  const m = Math.round(secs / 60);
  return (m > 0 ? '+' : '') + m + ' min';
}

// late5_pct may be a 0..1 share or a 0..100 percentage; normalize to 0..1.
function share(v) {
  const n = num(v);
  if (n === null) return null;
  return n > 1 ? n / 100 : n;
}

/* ---- header ---- */

async function loadHeader() {
  const info = await fetchJSON('/api/stops/' + encodeURIComponent(stopId));
  const stop = (info && (info.stop || info.Stop)) || info || {};
  const name = pick(stop, 'name', 'Name') || stopId;
  document.getElementById('stop-name').textContent = name;
  document.title = name + ' · RideWatch';

  const badges = document.getElementById('stop-routes');
  badges.textContent = '';
  for (const r of pickArray(info, 'routes', 'Routes')) {
    const label = pick(r, 'short_name', 'ShortName', 'route_short_name') ||
      pick(r, 'long_name', 'LongName') || pick(r, 'route_id', 'RouteID') || '?';
    const badge = el('span', 'badge', String(label));
    const color = pick(r, 'color', 'Color');
    const textColor = pick(r, 'text_color', 'TextColor');
    if (typeof color === 'string' && /^[0-9a-fA-F]{6}$/.test(color)) {
      badge.style.background = '#' + color;
      badge.style.color = /^[0-9a-fA-F]{6}$/.test(textColor || '') ? '#' + textColor : '#0b1120';
    }
    badges.append(badge);
  }
}

/* ---- upcoming arrivals ---- */

function renderArrivals(events) {
  const box = document.getElementById('arrivals');
  box.textContent = '';
  if (!events.length) {
    box.append(el('p', 'muted', 'No upcoming arrivals right now.'));
    return;
  }
  for (const ev of events.slice(0, 12)) {
    const route = String(pick(ev, 'route_short_name', 'RouteShortName', 'route_id', 'RouteID') || '?');
    const headsign = String(pick(ev, 'headsign', 'Headsign') || '');
    const sched = parseTime(pick(ev, 'scheduled_arrival', 'ScheduledArrival'));
    const delay = num(pick(ev, 'delay_secs', 'DelaySecs', 'delay'));

    let cls = 'unknown', text = '—';
    if (delay !== null) {
      if (Math.abs(delay) < 60) { cls = 'ok'; text = 'on time'; }
      else if (delay > 0) { cls = 'bad'; text = '+' + Math.round(delay / 60) + ' min'; }
      else { cls = 'warn'; text = '−' + Math.round(-delay / 60) + ' min'; }
    }

    const row = el('div', 'arrival');
    const main = el('div', 'arrival-main');
    main.append(
      el('div', 'arrival-headsign', headsign || route),
      el('div', 'arrival-sched muted', sched ? 'scheduled ' + clockLabel(sched) : 'unscheduled')
    );
    row.append(el('span', 'badge', route), main, el('span', 'delta ' + cls, text));
    box.append(row);
  }
}

async function refreshUpcoming() {
  try {
    const data = await fetchJSON('/api/stops/' + encodeURIComponent(stopId) + '/upcoming');
    renderArrivals(pickArray(data, 'events', 'upcoming', 'arrivals'));
  } catch {
    const box = document.getElementById('arrivals');
    box.textContent = '';
    box.append(el('p', 'muted', 'Live arrivals unavailable.'));
  }
}

/* ---- track record ---- */

const DAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];

function renderSentences(sentences) {
  const box = document.getElementById('sentences');
  box.textContent = '';
  if (!sentences.length) {
    box.append(el('p', 'muted', 'Not enough observations here yet — check back in a few days.'));
    return;
  }
  for (const s of sentences) box.append(el('p', null, String(s)));
}

function renderHeatmap(hourly) {
  // hour_of_week 0 = Monday 00:00 … 167 = Sunday 23:00 (agency tz), so
  // row = hour_of_week / 24 (day) and column = hour_of_week % 24 (hour).
  // A stop can have several routes/directions per hour; merge them weighted by n.
  const cells = Array.from({ length: 7 }, () => new Array(24).fill(null));
  for (const row of hourly) {
    const how = num(pick(row, 'hour_of_week', 'HourOfWeek'));
    const n = num(pick(row, 'n', 'N')) || 0;
    if (how === null || how < 0 || how > 167 || n <= 0) continue;
    const day = Math.floor(how / 24), hour = how % 24;
    const c = cells[day][hour] || (cells[day][hour] = { n: 0, late: 0, p50: 0, p50n: 0, p90: 0, p90n: 0 });
    c.n += n;
    c.late += (share(pick(row, 'late5_pct', 'Late5Pct')) || 0) * n;
    const p50 = num(pick(row, 'p50_delay_secs', 'P50DelaySecs'));
    if (p50 !== null) { c.p50 += p50 * n; c.p50n += n; }
    const p90 = num(pick(row, 'p90_delay_secs', 'P90DelaySecs'));
    if (p90 !== null) { c.p90 += p90 * n; c.p90n += n; }
  }

  const grid = document.getElementById('heatmap');
  grid.textContent = '';
  grid.append(el('div', 'hm-label'));
  for (let h = 0; h < 24; h++) {
    const lab = el('div', 'hm-label hm-h' + (h % 6 === 0 ? ' hm-h6' : ''), h % 3 === 0 ? String(h) : '');
    grid.append(lab);
  }
  for (let day = 0; day < 7; day++) {
    grid.append(el('div', 'hm-label', DAYS[day]));
    for (let hour = 0; hour < 24; hour++) {
      const c = cells[day][hour];
      const cell = el('div', 'hm-cell' + (c ? '' : ' empty'));
      if (c) {
        const late = c.late / c.n; // 0..1 share of late-5+ observations
        cell.style.background = 'rgba(248,113,113,' + Math.min(1, 0.08 + late * 1.4).toFixed(3) + ')';
        const parts = [DAYS[day] + ' ' + String(hour).padStart(2, '0') + ':00',
          Math.round(late * 100) + '% late 5+ min'];
        if (c.p50n) parts.push('p50 ' + fmtDelayMin(c.p50 / c.p50n));
        if (c.p90n) parts.push('p90 ' + fmtDelayMin(c.p90 / c.p90n));
        parts.push('n=' + c.n);
        cell.title = parts.join(' · ');
      } else {
        cell.title = DAYS[day] + ' ' + String(hour).padStart(2, '0') + ':00 · no data';
      }
      grid.append(cell);
    }
  }
}

function renderWorst(departures) {
  const body = document.getElementById('worst');
  body.textContent = '';
  const rows = departures
    .map(d => ({
      secs: num(pick(d, 'scheduled_secs', 'ScheduledSecs')),
      route: String(pick(d, 'route_short_name', 'route_id', 'RouteID') || '?'),
      dayClass: String(pick(d, 'day_class', 'DayClass') || ''),
      late5: share(pick(d, 'late5_pct', 'Late5Pct')),
      p90: num(pick(d, 'p90_delay_secs', 'P90DelaySecs')),
    }))
    .filter(r => r.secs !== null && r.late5 !== null)
    .sort((a, b) => b.late5 - a.late5)
    .slice(0, 10);
  if (!rows.length) {
    const tr = el('tr');
    const td = el('td', 'muted', 'No departure stats yet.');
    td.colSpan = 4;
    tr.append(td);
    body.append(tr);
    return;
  }
  for (const r of rows) {
    const tr = el('tr');
    const time = el('td', null, gtfsClock(r.secs));
    if (r.dayClass) time.append(el('span', 'sub', r.dayClass));
    tr.append(
      time,
      el('td', null, r.route),
      el('td', 'num', Math.round(r.late5 * 100) + '%'),
      el('td', 'num', r.p90 !== null ? fmtDelayMin(r.p90) : '—')
    );
    body.append(tr);
  }
}

async function loadReliability() {
  try {
    const rel = await fetchJSON('/api/stops/' + encodeURIComponent(stopId) + '/reliability');
    renderSentences(pickArray(rel && (rel.sentences || rel.Sentences)));
    renderHeatmap(pickArray(rel, 'hourly', 'Hourly', 'stop_hourly'));
    renderWorst(pickArray(rel, 'departures', 'Departures', 'worst_departures'));
  } catch {
    const box = document.getElementById('sentences');
    box.textContent = '';
    box.append(el('p', 'muted', 'Track record unavailable.'));
    renderHeatmap([]);
    renderWorst([]);
  }
}

/* ---- notify me ---- */

const SUB_KEY = 'ridewatch.sub.' + stopId;
const notifyBtn = document.getElementById('notify-btn');
const notifyStatus = document.getElementById('notify-status');
const thresholds = document.getElementById('thresholds');

const pushSupported =
  'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;

function savedSub() {
  try { return JSON.parse(localStorage.getItem(SUB_KEY) || 'null'); }
  catch { return null; }
}

function chosenThreshold() {
  const r = thresholds.querySelector('input[name=threshold]:checked');
  return r ? Number(r.value) : 300;
}

function renderNotify() {
  const saved = savedSub();
  if (saved) {
    const t = thresholds.querySelector('input[value="' + saved.threshold_secs + '"]');
    if (t) t.checked = true;
    notifyBtn.textContent = 'Turn off alerts';
    notifyBtn.classList.add('off');
    notifyStatus.textContent =
      'Alerts on: delays of ' + Math.round(saved.threshold_secs / 60) + '+ minutes at this stop.';
  } else {
    notifyBtn.textContent = 'Turn on alerts';
    notifyBtn.classList.remove('off');
    notifyStatus.textContent = '';
  }
}

// VAPID keys arrive base64url-encoded; PushManager wants the raw bytes.
function base64urlToUint8Array(s) {
  const pad = '='.repeat((4 - (s.length % 4)) % 4);
  const b64 = (s + pad).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(b64);
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}

async function postSubscription(sub, thresholdSecs) {
  const body = sub.toJSON(); // {endpoint, keys: {p256dh, auth}}
  const res = await fetch('/api/subscriptions', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      endpoint: body.endpoint,
      keys: body.keys,
      stop_id: stopId,
      threshold_secs: thresholdSecs,
    }),
  });
  if (!res.ok) throw new Error('subscription rejected: HTTP ' + res.status);
  localStorage.setItem(SUB_KEY, JSON.stringify({
    endpoint: body.endpoint,
    threshold_secs: thresholdSecs,
  }));
}

async function subscribe() {
  const perm = await Notification.requestPermission();
  if (perm !== 'granted') {
    notifyStatus.textContent = 'Notifications are blocked for this site.';
    return;
  }
  const reg = await navigator.serviceWorker.register('/sw.js');
  await navigator.serviceWorker.ready;
  const vapid = await fetchJSON('/api/vapid-public-key');
  const key = pick(vapid, 'key', 'Key');
  if (typeof key !== 'string') throw new Error('no VAPID key from server');
  const sub = await reg.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: base64urlToUint8Array(key),
  });
  await postSubscription(sub, chosenThreshold());
}

async function unsubscribe() {
  const reg = await navigator.serviceWorker.getRegistration('/sw.js');
  const sub = reg && await reg.pushManager.getSubscription();
  const saved = savedSub();
  const endpoint = (sub && sub.endpoint) || (saved && saved.endpoint);
  if (sub) await sub.unsubscribe();
  if (endpoint) {
    await fetch('/api/subscriptions', {
      method: 'DELETE',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ endpoint }),
    });
  }
  localStorage.removeItem(SUB_KEY);
}

function initNotify() {
  if (!pushSupported) {
    thresholds.disabled = true;
    notifyBtn.disabled = true;
    notifyStatus.textContent = 'Push notifications are not supported in this browser.';
    return;
  }
  renderNotify();

  notifyBtn.addEventListener('click', async () => {
    notifyBtn.disabled = true;
    try {
      if (savedSub()) await unsubscribe();
      else await subscribe();
      renderNotify();
    } catch (err) {
      notifyStatus.textContent = 'Could not update alerts: ' + err.message;
    } finally {
      notifyBtn.disabled = false;
    }
  });

  // Changing the threshold while subscribed re-posts the same endpoint,
  // which updates the stored threshold server-side.
  thresholds.addEventListener('change', async () => {
    if (!savedSub()) return;
    try {
      const reg = await navigator.serviceWorker.getRegistration('/sw.js');
      const sub = reg && await reg.pushManager.getSubscription();
      if (sub) {
        await postSubscription(sub, chosenThreshold());
        renderNotify();
      }
    } catch { /* keep the old threshold */ }
  });
}

/* ---- boot ---- */

if (!stopId || stopId === 'stop') {
  document.getElementById('stop-name').textContent = 'Stop not found';
} else {
  loadHeader().catch(() => {
    document.getElementById('stop-name').textContent = stopId;
  });
  refreshUpcoming();
  setInterval(() => { if (!document.hidden) refreshUpcoming(); }, UPCOMING_MS);
  loadReliability();
  initNotify();
}
