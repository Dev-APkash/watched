---
name: Watched
tagline: A private shelf of everything you've watched, kept in a cabin on a rainy evening
aesthetic: Cabin Evening (illustrated)
mood: Cozy, hand-drawn, lamplit, unhurried
surface: Animated illustrated log-cabin backdrop (rain on the window, fire in the hearth, steam off the kettle) with cream paper UI drawn in the same cocoa ink line
density: 4-to-8-column bookcase under a floating control plank
font_display: Young Serif
font_ui: Figtree
font_hand: Caveat
color_bg: "#2a1810"
color_text: "#3b2218"
color_accent: "#e57a35"
---

# Watched Design System

## Overview

Watched puts your watched history on a bookcase inside an illustrated log cabin. The room is the backdrop: a hand-drawn cabin interior, animated (rain on the window, the hearth fire, kettle steam), fixed behind the page. Everything you interact with is drawn in the illustration's own hand: flat cream paper, a 2–3px cocoa ink outline, cel-shaded with one hard tonal band instead of glossy gradients.

On arrival the first viewport is mostly room. The nameplate hangs on the wall from a nail, the fire and couch stay visible, and the bookcase starts just below them. Scrolling slides the shelf up over the scene, and the control plank sticks to the top.

**Sacred motion invariant:** every 3D case and disc interaction (shelf hover, pull-out arc, front/back flip, lid swing, disc pop-out and spin, drag inertia, wheel rotation) is governed by the CSS between `/* ---------- Slots` and `/* ---------- Stage` in `index.html` and by the script. UI restyles never touch either. The case's own printed typography (`--serif` Bodoni Moda, `--mono` Azeret Mono, `--sans` Schibsted Grotesk, `--hand` Caveat) and paper/plastic tokens (`--paper`, `--ink*`, `--rule`, `--accent`, `--plastic*`) belong to the case and stay as they are.

## The room

- **Backdrop:** `.scene` is a fixed `<picture>`: `assets/cabin.webp` (1920×1080 animated WebP, 27 frames, ~1.1s loop, ~680 KB, converted from the 4K `cabinimage.gif`). `assets/cabin-still.jpg` (frame 1) is served instead under `prefers-reduced-motion: reduce`, and is also the `body` background so the room paints before the WebP decodes.
- **Firelight:** `.scene-glow` is a soft warm radial gradient centered on the hearth (66% / 48%), screen-blended, with an irregular `steps()` opacity flicker (`@keyframes firelight`, 3.4s). Disabled under reduced motion.
- **Legibility shade:** `.scene::after` darkens the top and bottom edges plus a light vignette so cards and shelf separate from the picture. Keep it light, because the scene is the point.
- Assets are served by `GET /assets/{name}` in `main.go` (embedded with `//go:embed assets`, with a copy on disk taking precedence, like `index.html`).

## Colors

All colors are tokens on `:root`, sampled from the illustration.

| Token | Value | Role |
| :--- | :--- | :--- |
| `--line` | `#3b2218` | Cocoa ink outline on every card, button, and joinery edge; also text on cream (13:1) |
| `--cream` / `--cream-2` / `--cream-3` | `#f7e7cf` / `#efd7b6` / `#e2c195` | Paper card fill / secondary button + inset fill / inactive stars, scroll thumbs |
| `--t1` / `--t2` / `--t3` | `#3b2218` / `#6a4634` / `#7f5a46` | Ink on cream: primary / secondary (6.9:1) / quiet labels (5:1) |
| `--wood-hi` / `--wood` / `--wood-lo` | `#a46c47` / `#7d4f35` / `#5e3a27` | Bookcase: lit shelf top / boards / plinth + grain strokes |
| `--bay` / `--bay-shade` | `#3b2419` / `#2a1810` | Bookcase interior / cel shadow band and page base |
| `--sofa` | `#4a5890` | Selected state (segmented filter, pressed toggles, sound on), input focus; cream on it 5.6:1 |
| `--ember` / `--ember-hi` | `#e57a35` / `#f0964f` | Primary action (Pick one for me, Stream on Jellyfin) with ink text (5:1), filled stars |
| `--flame` | `#f6c453` | Hover fill for secondary buttons, focus ring, highlighter swipe behind ledger numbers, selection |
| `--leaf` | `#6f9a4f` | Sync OK / Seerr linked dot |

## Typography

| Role | Face | Use |
| :--- | :--- | :--- |
| `--display` | Young Serif 400 | Wordmark (50px; 40px mobile), panel headings (17px), stage title (21px), empty state (24px) |
| `--ui` | Figtree 400–800 | All controls and body copy. Buttons 13–14px / 700–800. Numbers use `tabular-nums` |
| `--hand` | Caveat 600 | The owner's name under the wordmark, liner-notes textarea, "room for more" shelf note |

No monospace in the room chrome. Monospace stays on the case insert where it is printed data.

## Components

- **Nameplate (`.nameplate`):** cream card, 2.5px ink outline, 14px radius, hung from a nail by an SVG string (`::before`). Holds the wordmark, the handwritten owner, and the ledger sentence. Ledger numbers get a flame highlighter swipe (`background` band at 55–92% height).
- **Sync card (`.sync`):** the same card language holding the status dot and the `text-btn`s.
- **Buttons:** fills are flat, outlines are ink, and depth is an inner bottom cel shade (`inset 0 -3px 0 rgba(59,34,24,.16)`), never a hard block shadow. Hover lifts 1px and fills flame (secondary) or ember-hi (primary); active presses 1px down.
- **Control plank (`.toolbar` / `.toolbar-in`):** a floating cream bar, 16px radius, sticky at `top: 12px`. Its top margin (`clamp(24px, calc(56vh - 196px), 360px)`) is what keeps the fire in view on arrival.
- **Bookcase:** drawn joinery. `.bc-cap` crown board, `.bc-body` side boards with hand-drawn grain strokes (inline SVG), `.shelf-back` bays with a hard cel shadow band under each shelf, `.plank-face` boards with ink top and bottom edges, cream paper date tags tilted -1.5°, `.bc-base` plinth with two feet.
- **Stage:** the veil dims the cabin to a warm, blurred room rather than black. Side panels and the bottom strip are cream paper cards. Section headings are Young Serif. Rows are separated by dashed or hairline ink rules (no nested cards). The notes textarea is ruled notepaper in Caveat.
- **Focus:** a 3px flame outline with 2px offset everywhere. Inputs focus with a sofa-blue border plus a 3px soft sofa ring.

## Do / Don't

- **Do** draw new chrome with the ink outline + flat fill + inner cel shade recipe, and sample any new color from the illustration.
- **Do** keep the first viewport room-forward. If the header grows, rebalance the toolbar margin so the hearth stays visible at 1440×900.
- **Don't** edit anything between the Slots and Stage comments in the CSS, or the case tokens and fonts listed above.
- **Don't** use hard offset block shadows, glows, glass panels, or gradient text. Don't nest cards.
- **Don't** darken the scene overlay past what text separation needs.
