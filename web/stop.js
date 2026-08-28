/* RideWatch stop page: header, live arrivals, track record, push alerts. */

import { delayClass, delayText, DELAY_COLORS } from '/delay.js';

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
  return (m < 0 ? '−' + -m : '+' + m) + ' min';
}

// late5_pct may be a 0..1 share or a 0..100 percentage; normalize to 0..1.
function share(v) {
  const n = num(v);
  if (n === null) return null;
  return n > 1 ? n / 100 : n;
}

// textOn picks readable text for an agency-supplied badge color: GTFS feeds
// often omit route_text_color, and a dark-on-dark badge is unreadable.
function textOn(hex6) {
  const n = parseInt(hex6, 16);
  const lin = v => {
    v /= 255;
    return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
  };
  const l = 0.2126 * lin((n >> 16) & 255) + 0.7152 * lin((n >> 8) & 255) + 0.0722 * lin(n & 255);
  return l > 0.35 ? 'var(--bg)' : '#ffffff';
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
      badge.style.color = /^[0-9a-fA-F]{6}$/.test(textColor || '') ? '#' + textColor : textOn(color);
    }
    badges.append(badge);
  }
}

/* ---- upcoming arrivals ---- */

// dueLabel: minutes until the prediction — the number a waiting rider wants.
function dueLabel(predicted, now) {
  const mins = Math.round((predicted - now) / 60000);
  return mins <= 0 ? 'Due' : mins + ' min';
}

function renderArrivals(events) {
  const box = document.getElementById('arrivals');
  box.textContent = '';
  if (!events.length) {
    box.append(el('p', 'muted', 'No upcoming arrivals right now.'));
    return;
  }
  const now = new Date();
  for (const ev of events.slice(0, 12)) {
    const route = String(pick(ev, 'route_short_name', 'RouteShortName', 'route_id', 'RouteID') || '?');
    const headsign = String(pick(ev, 'headsign', 'Headsign') || '');
    const sched = parseTime(pick(ev, 'scheduled', 'scheduled_arrival', 'ScheduledArrival'));
    const predicted = parseTime(pick(ev, 'predicted', 'predicted_arrival', 'PredictedArrival'));
    const delay = num(pick(ev, 'delay_secs', 'DelaySecs', 'delay'));

    const row = el('div', 'arrival');
    const main = el('div', 'arrival-main');
    main.append(el('div', 'arrival-headsign', headsign ? 'to ' + headsign : 'Route ' + route));
    const bits = [];
    if (predicted) bits.push('arriving ' + clockLabel(predicted));
    bits.push(sched ? 'scheduled ' + clockLabel(sched) : 'extra trip');
    main.append(el('div', 'arrival-sched muted', bits.join(' · ')));

    const right = el('div', 'arrival-right');
    right.append(
      el('div', 'due', predicted ? dueLabel(predicted, now) : '—'),
      el('span', 'delta ' + delayClass(delay), delayText(delay))
    );

    row.append(el('span', 'badge', route), main, right);
    box.append(row);
  }
}

// Freshness: the "Now" list self-refreshes; say so, or live and stale look identical.
let lastUpdatedAt = null;
function renderFreshness() {
  const out = document.getElementById('now-updated');
  if (!out || !lastUpdatedAt) return;
  const s = Math.round((Date.now() - lastUpdatedAt) / 1000);
  out.textContent = s < 10 ? 'updated just now' : s < 90 ? 'updated ' + s + 's ago'
    : 'updated ' + Math.round(s / 60) + ' min ago';
}
setInterval(renderFreshness, 5000);

async function refreshUpcoming() {
  try {
    const data = await fetchJSON('/api/stops/' + encodeURIComponent(stopId) + '/upcoming');
    renderArrivals(pickArray(data, 'events', 'upcoming', 'arrivals'));
    lastUpdatedAt = Date.now();
    renderFreshness();
  } catch (err) {
    const box = document.getElementById('arrivals');
    box.textContent = '';
    box.append(el('p', 'muted', /404/.test(err && err.message)
      ? 'This stop is not in the current schedule. It may have moved or been renamed.'
      : 'Live arrivals unavailable.'));
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
  // --bad as r,g,b so the ramp always matches the palette.
  const badN = parseInt(DELAY_COLORS.bad.replace('#', ''), 16) || 0xf87171;
  const badRGB = [(badN >> 16) & 255, (badN >> 8) & 255, badN & 255].join(',');
  const detail = document.getElementById('heatmap-detail');

  for (let day = 0; day < 7; day++) {
    grid.append(el('div', 'hm-label', DAYS[day]));
    for (let hour = 0; hour < 24; hour++) {
      const c = cells[day][hour];
      const cell = el('div', 'hm-cell' + (c ? '' : ' empty'));
      const when = DAYS[day] + ' ' + String(hour).padStart(2, '0') + ':00';
      let text = when + ' · no data';
      if (c) {
        const late = c.late / c.n; // 0..1 share of late-5+ observations
        // A reliable hour stays neutral; the ramp tops out at 100%, so 60%
        // and 100% no longer look the same.
        if (late > 0) {
          cell.style.background = 'rgba(' + badRGB + ',' + Math.min(0.92, 0.12 + late * 0.8).toFixed(3) + ')';
        }
        const parts = [when, Math.round(late * 100) + '% of trips 5+ min late'];
        if (c.p50n) parts.push('typically ' + fmtDelayMin(c.p50 / c.p50n));
        if (c.p90n) parts.push('worst case ' + fmtDelayMin(c.p90 / c.p90n));
        parts.push(c.n + ' observations');
        text = parts.join(' · ');
      }
      cell.title = text;
      // Tooltips don't exist on touch; a tap writes the detail line instead.
      cell.addEventListener('click', () => { detail.textContent = text; });
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
  const history = document.getElementById('history');
  try {
    const rel = await fetchJSON('/api/stops/' + encodeURIComponent(stopId) + '/reliability');
    const hourly = pickArray(rel, 'hourly', 'Hourly', 'stop_hourly');
    const departures = pickArray(rel, 'departures', 'Departures', 'worst_departures');
    renderSentences(pickArray(rel && (rel.sentences || rel.Sentences)));
    // An empty grid and an empty table are noise, not information.
    if (hourly.length || departures.length) {
      history.hidden = false;
      renderHeatmap(hourly);
      renderWorst(departures);
    } else {
      history.hidden = true;
    }
  } catch {
    const box = document.getElementById('sentences');
    box.textContent = '';
    box.append(el('p', 'muted', 'Track record unavailable.'));
    history.hidden = true;
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

async function initNotify() {
  if (!pushSupported) {
    thresholds.disabled = true;
    notifyBtn.disabled = true;
    notifyStatus.textContent = 'Push notifications are not supported in this browser.';
    return;
  }
  // Don't offer a button that can only fail: hide the controls when the
  // server runs without VAPID keys.
  try {
    const info = await fetchJSON('/api/feedinfo');
    if (pick(info, 'push_enabled', 'PushEnabled') === false) {
      thresholds.hidden = true;
      notifyBtn.hidden = true;
      notifyStatus.textContent = 'Alerts are switched off on this server.';
      return;
    }
  } catch { /* can't tell — leave the controls; subscribe reports its own errors */ }
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
