/* RideWatch map page: live vehicles, stop search, viewport stops, feed status. */

import { delayClass, delayText, delayColor } from '/delay.js';

const VEHICLES_MS = 10000;
const FEEDINFO_MS = 30000;
const STOPS_MIN_ZOOM = 14;
const EMPTY = { type: 'FeatureCollection', features: [] };

async function fetchJSON(url, opts) {
  const res = await fetch(url, opts);
  if (!res.ok) throw new Error(url + ': HTTP ' + res.status);
  return res.json();
}

function debounce(fn, ms) {
  let t;
  return (...args) => { clearTimeout(t); t = setTimeout(() => fn(...args), ms); };
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

function num(v) {
  const n = Number(v);
  return v === null || v === undefined || !Number.isFinite(n) ? null : n;
}

/* ---- status pill ---- */

const pill = document.getElementById('pill');
let feedOk = false;
let vehiclesOk = true;
let worstAgeSecs = null;

// Feed ages may arrive as {feeds:{name:{age_secs}}}, {feeds:[{age_secs}]}, or
// bare numbers; collect whatever looks like an age and judge by the worst one.
function extractAges(info) {
  const ages = [];
  const push = v => { const n = num(v); if (n !== null && n >= 0) ages.push(n); };
  const feeds = pick(info, 'feeds', 'feed_ages', 'ages');
  if (Array.isArray(feeds)) {
    for (const f of feeds) push(pick(f, 'age_secs', 'age_seconds', 'age'));
  } else if (feeds && typeof feeds === 'object') {
    for (const v of Object.values(feeds)) {
      if (typeof v === 'number') push(v);
      else push(pick(v, 'age_secs', 'age_seconds', 'age'));
    }
  }
  return ages;
}

function renderPill() {
  let cls = 'pill-red';
  let text = 'no data — retrying';
  if (feedOk && vehiclesOk && worstAgeSecs === null) {
    // Reached the server but no feed age yet: connecting, not broken.
    cls = '';
    text = 'connecting…';
  } else if (feedOk && vehiclesOk && worstAgeSecs !== null) {
    const s = worstAgeSecs;
    const label = s < 90 ? Math.round(s) + 's' : Math.round(s / 60) + 'm';
    if (s < 60) { cls = 'pill-green'; text = 'live · ' + label; }
    else if (s < 180) { cls = 'pill-amber'; text = 'lagging · ' + label; }
    else { text = 'stale · ' + label; }
  }
  pill.className = ('pill ' + cls).trim();
  pill.textContent = text;
}

/* ---- legend note: vehicle count + how to reach stops ---- */

const legendNote = document.getElementById('legend-note');
let vehicleCount = null;

function renderLegendNote() {
  if (!legendNote) return;
  const parts = [];
  if (vehicleCount !== null) {
    parts.push(vehicleCount === 0 ? 'no vehicles reporting' : vehicleCount + ' vehicles');
  }
  parts.push(map.getZoom() < STOPS_MIN_ZOOM ? 'zoom in for stops' : 'tap a stop for arrivals');
  legendNote.textContent = parts.join(' · ');
}

async function refreshFeedInfo() {
  try {
    const info = await fetchJSON('/api/feedinfo');
    const ages = extractAges(info);
    worstAgeSecs = ages.length ? Math.max(...ages) : null;
    feedOk = true;
  } catch {
    feedOk = false;
  }
  renderPill();
}

/* ---- map ---- */

let tileUrl = 'https://tile.openstreetmap.org/{z}/{x}/{y}.png';
try {
  const info = await fetchJSON('/api/feedinfo');
  const t = pick(info, 'tile_url', 'tileUrl', 'TileURL');
  if (typeof t === 'string') tileUrl = t;
  const ages = extractAges(info);
  worstAgeSecs = ages.length ? Math.max(...ages) : null;
  feedOk = true;
} catch {
  feedOk = false;
}
renderPill();

const map = new maplibregl.Map({
  container: 'map',
  style: {
    version: 8,
    sources: {
      osm: {
        type: 'raster',
        tiles: [tileUrl],
        tileSize: 256,
        maxzoom: 19,
        attribution: '&copy; OpenStreetMap contributors',
      },
    },
    layers: [{ id: 'osm', type: 'raster', source: 'osm' }],
  },
  center: [-71.06, 42.355], // MBTA-ish default; first vehicle payload refits
  zoom: 12,
});
map.addControl(new maplibregl.NavigationControl({ showCompass: false }), 'top-right');

/* ---- vehicles ---- */

// Flatten popup fields onto each feature up front: event feature properties
// round-trip through JSON, so precomputed scalar strings are the safe path.
function annotateVehicles(data) {
  const fc = data && data.type === 'FeatureCollection' ? data : EMPTY;
  for (const f of fc.features || []) {
    if (!f || !f.geometry || f.geometry.type !== 'Point') continue;
    const p = f.properties || (f.properties = {});
    const d = num(pick(p, 'delay_secs', 'DelaySecs', 'delay'));
    p._color = delayColor(d);
    p._delay_class = delayClass(d);
    p._delay_text = delayText(d);
    p._route = String(pick(p, 'route_short_name', 'RouteShortName', 'route', 'route_id', 'RouteID') || '?');
    p._headsign = String(pick(p, 'headsign', 'Headsign') || '');
  }
  return fc;
}

let didFit = false;
function maybeFit(fc) {
  if (didFit || !fc.features.length) return;
  didFit = true;
  const b = new maplibregl.LngLatBounds();
  for (const f of fc.features) {
    if (f.geometry && f.geometry.type === 'Point') b.extend(f.geometry.coordinates);
  }
  if (!b.isEmpty()) map.fitBounds(b, { padding: 48, maxZoom: 13, duration: 0 });
}

async function refreshVehicles() {
  try {
    const fc = annotateVehicles(await fetchJSON('/api/vehicles'));
    const src = map.getSource('vehicles');
    if (src) src.setData(fc);
    maybeFit(fc);
    vehicleCount = fc.features.length;
    vehiclesOk = true;
  } catch {
    vehiclesOk = false;
  }
  renderPill();
  renderLegendNote();
}

/* ---- stops in viewport ---- */

function stopsToGeoJSON(data) {
  if (data && data.type === 'FeatureCollection') return data;
  const list = Array.isArray(data) ? data : (data && (data.stops || data.Stops)) || [];
  const features = [];
  for (const s of list) {
    const lat = num(pick(s, 'lat', 'Lat'));
    const lon = num(pick(s, 'lon', 'Lon', 'lng'));
    const id = pick(s, 'stop_id', 'StopID', 'id');
    if (lat === null || lon === null || !id) continue;
    features.push({
      type: 'Feature',
      geometry: { type: 'Point', coordinates: [lon, lat] },
      properties: { stop_id: String(id), name: String(pick(s, 'name', 'Name') || '') },
    });
  }
  return { type: 'FeatureCollection', features };
}

const refreshStops = debounce(async () => {
  const src = map.getSource('stops');
  if (!src) return;
  if (map.getZoom() < STOPS_MIN_ZOOM) { src.setData(EMPTY); return; }
  const b = map.getBounds();
  const bbox = [b.getWest(), b.getSouth(), b.getEast(), b.getNorth()]
    .map(n => n.toFixed(5)).join(',');
  try {
    src.setData(stopsToGeoJSON(await fetchJSON('/api/stops?bbox=' + bbox)));
  } catch { /* stale dots beat a broken map */ }
}, 300);

/* ---- layers + interactions ---- */

map.on('load', () => {
  map.addSource('vehicles', { type: 'geojson', data: EMPTY });
  map.addSource('stops', { type: 'geojson', data: EMPTY });

  map.addLayer({
    id: 'vehicles',
    type: 'circle',
    source: 'vehicles',
    paint: {
      'circle-radius': ['interpolate', ['linear'], ['zoom'], 9, 4, 16, 9],
      'circle-color': ['get', '_color'],
      'circle-stroke-width': 1.5,
      'circle-stroke-color': '#0b1120',
      'circle-opacity': 0.95,
    },
  });
  map.addLayer({
    id: 'stops',
    type: 'circle',
    source: 'stops',
    minzoom: STOPS_MIN_ZOOM,
    paint: {
      'circle-radius': 4.5,
      'circle-color': '#0b1120',
      'circle-stroke-width': 2,
      'circle-stroke-color': '#94a3b8',
    },
  }, 'vehicles'); // keep vehicles drawn above stop dots

  map.on('click', 'vehicles', e => {
    const p = e.features[0].properties;
    const div = document.createElement('div');
    const route = document.createElement('div');
    route.className = 'popup-route';
    route.textContent = p._route + (p._headsign ? ' → ' + p._headsign : '');
    const delay = document.createElement('div');
    delay.className = 'popup-delay ' + p._delay_class;
    delay.textContent = p._delay_text;
    div.append(route, delay);
    new maplibregl.Popup({ closeButton: false, offset: 10 })
      .setLngLat(e.features[0].geometry.coordinates)
      .setDOMContent(div)
      .addTo(map);
  });

  map.on('click', 'stops', e => {
    const id = e.features[0].properties.stop_id;
    if (id) location.href = '/stop/' + encodeURIComponent(id);
  });

  for (const layer of ['vehicles', 'stops']) {
    map.on('mouseenter', layer, () => { map.getCanvas().style.cursor = 'pointer'; });
    map.on('mouseleave', layer, () => { map.getCanvas().style.cursor = ''; });
  }

  map.on('moveend', refreshStops);
  map.on('zoom', renderLegendNote);
  renderLegendNote();
  refreshVehicles();
  refreshStops();
  setInterval(() => { if (!document.hidden) refreshVehicles(); }, VEHICLES_MS);
  setInterval(() => { if (!document.hidden) refreshFeedInfo(); }, FEEDINFO_MS);
});

/* ---- stop search ---- */

const searchInput = document.getElementById('search');
const searchResults = document.getElementById('search-results');

function hideResults() {
  searchResults.hidden = true;
  searchResults.textContent = '';
}

function showResults(stops) {
  searchResults.textContent = '';
  if (!stops.length) { hideResults(); return; }
  for (const s of stops.slice(0, 10)) {
    const lat = num(pick(s, 'lat', 'Lat'));
    const lon = num(pick(s, 'lon', 'Lon', 'lng'));
    const li = document.createElement('li');
    li.textContent = pick(s, 'name', 'Name') || pick(s, 'stop_id', 'StopID') || '?';
    li.addEventListener('click', () => {
      hideResults();
      searchInput.blur();
      if (lat !== null && lon !== null) map.flyTo({ center: [lon, lat], zoom: 16 });
    });
    searchResults.append(li);
  }
  searchResults.hidden = false;
}

const runSearch = debounce(async () => {
  const q = searchInput.value.trim();
  if (q.length < 2) { hideResults(); return; }
  try {
    const data = await fetchJSON('/api/stops?q=' + encodeURIComponent(q));
    const stops = Array.isArray(data) ? data : (data && (data.stops || data.Stops)) || [];
    showResults(stops);
  } catch {
    hideResults();
  }
}, 250);

searchInput.addEventListener('input', runSearch);
searchInput.addEventListener('keydown', e => {
  if (e.key === 'Escape') hideResults();
  if (e.key === 'Enter') {
    const first = searchResults.querySelector('li');
    if (first) first.click();
  }
});
document.addEventListener('click', e => {
  if (!e.target.closest('.search-wrap')) hideResults();
});
