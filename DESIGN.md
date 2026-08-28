# RideWatch design system

The rendered truth lives in `web/style.css`; this file records the decisions so
they survive contributors and redesign moods.

## Voice

A transit utility. Calm surfaces, dense but readable data, zero decoration.
Every pixel either helps a rider decide ("when's my bus, is it late, do I run")
or gets out of the way.

## Type

- System font stack, deliberately: instant load, native feel on a tool people
  open at a bus stop with one bar of signal. Revisit only with a real brand pass.
- 16px body. Nothing below 12px (`--fs-caption`).
- `tabular-nums` on every column of numbers (countdowns, deltas, tables).

## Color

Single source: the CSS custom properties in `:root`. JavaScript reads them via
`getComputedStyle` (see `web/delay.js`) — never re-declare a palette value in JS.

Delay semantics (shared by the map, the stop page, and the legend):

| state | token | meaning |
|---|---|---|
| ok | `--ok` | on time, < 2 min late |
| warn | `--warn` | 2–5 min late |
| bad | `--bad` | more than 5 min late |
| early | `--early` | ≥ 1 min early — a rider might miss it; it is not "good" |
| unknown | `--unknown` | no estimate |

The classification lives in `web/delay.js` and nowhere else.

## Spacing & shape

- Scale: `--s1…--s5` (4/8/12/16/24px). Legacy literals migrate as files are touched.
- Radii: `--r1…--r3` + pill. Boxes are rare: the stop page is a document with
  section rules; the one panel is the notify block, because it *is* an interaction.

## Interaction

- Touch targets ≥ 44px. Focus ring: 2px `--accent` via `:focus-visible`.
- Control boundaries use `--border-strong`; passive separators use `--border`.
- `[hidden]` always wins (`display: none !important`) — class display rules must
  not resurrect hidden elements.
- Motion: exactly three quiet behaviors (hover/press feedback, the search
  dropdown settling in, the live dot breathing), all inside
  `prefers-reduced-motion: no-preference`. Nothing animates layout.

## Map

- Vehicles: delay-colored dots; bearing drawn as an SDF chevron on the dot's edge.
- Stops: dark dots with gray strokes, zoom ≥ 14, 12px padded tap area, hover names.
- The legend (bottom-left) must always decode every color on the canvas.
- The map is optional: WebGL failure degrades to a notice; search, the status
  pill, and stop pages keep working.

## Words

- Plain rider language: "worst case", "extra trip", "no data". No p90, no
  "unscheduled", no unexplained jargon.
- Delay labels are words ("3 min late", "2 min early", "on time"), identical
  everywhere a delay appears.
