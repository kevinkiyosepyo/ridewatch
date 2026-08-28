/* The one place delay seconds become classes, words, and colors.
   Thresholds follow the product's own definition (style.css tokens, the
   "Late 5+ min" stats, the 3/5/10-minute alert thresholds):
     early    <= -60s   (you might miss it — that's not "on time")
     ok       < 120s late
     warn     2–5 min late
     bad      > 5 min late
     unknown  no estimate */

export function delayClass(d) {
  if (d === null || d === undefined) return 'unknown';
  if (d <= -60) return 'early';
  if (d < 120) return 'ok';
  if (d <= 300) return 'warn';
  return 'bad';
}

export function delayText(d) {
  if (d === null || d === undefined) return 'no data';
  if (d <= -60) return Math.round(-d / 60) + ' min early';
  if (Math.abs(d) < 60) return 'on time';
  return Math.round(d / 60) + ' min late';
}

// Colors come from the stylesheet, so CSS stays the single source of truth
// (MapLibre paint properties need literal values).
const css = getComputedStyle(document.documentElement);
const v = name => css.getPropertyValue(name).trim() || '#9ca3af';
export const DELAY_COLORS = {
  early: v('--early'),
  ok: v('--ok'),
  warn: v('--warn'),
  bad: v('--bad'),
  unknown: v('--unknown'),
};

export function delayColor(d) {
  return DELAY_COLORS[delayClass(d)];
}
