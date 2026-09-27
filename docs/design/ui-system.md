# Dashboard design system

**Subject:** a self-hosted control panel for machines that AI agents use.
**Audience:** developers who run it on their own servers.
**Primary job:** see which boxes exist and what state they're in, then act (create, open desktop, stop, resume, delete), and manage nodes and settings.
**Tone:** calm, precise and professional, like a good infrastructure console. No decoration that doesn't carry information.

## Color (CSS custom properties; light is default, dark via prefers-color-scheme + a manual toggle)
| Token | Light | Dark | Use |
|---|---|---|---|
| --bg | #F4F5F7 | #15171B | page background |
| --surface | #FFFFFF | #1C1F24 | panels, tables, inputs |
| --surface-sunken | #EEF0F3 | #121418 | table header, code blocks |
| --line | #E3E5E9 | #2A2E35 | 1px borders, row dividers |
| --ink | #16181D | #E8EAEE | text, primary button fill |
| --muted | #5E6470 | #9AA1AD | secondary text |
| --link | #2B59C3 | #7D9CF2 | links, focus ring (2px) |
| --running | #1F8A5B | #45B383 | state: ready/running/idle |
| --starting | #B7791F | #D9A24A | state: provisioning/resuming/stopping |
| --error | #C8412F | #E4705F | state: error/unreachable; destructive buttons |
| --stopped | #8A8F98 | #7A808A | state: stopped |
No gradients, no glows, no box shadows (except the one used by menus/dialogs: 0 8px 24px rgba(22,24,29,.12)). No other accent colors.

## Type
- IBM Plex Sans (400, 500, 600) for the whole UI; IBM Plex Mono (400) only for real identifiers: box/node IDs, IPs, hostnames, commands, keys. Load with next/font/google (self-hosted at build).
- Scale (px / line-height): 12/16 (meta), 14/20 (body, default), 16/24 (emphasis), 20/28 (page title), 28/36 (auth title only). Weights: 400 body, 500 labels/buttons, 600 titles.
- Sentence case everywhere. Never all-caps labels, never letter-spaced eyebrows, never italic or colored accent words in headings, never "·"-joined meta strings, never arrows (→ ↗) in buttons or links.

## Components
- Buttons: height 40px (small: 32px), padding 0 16px, radius 6px, 14px/500. Primary = --ink fill with --surface text; secondary = --surface with 1px --line border and --ink text; danger = --error text on secondary style, filled --error only inside a confirm dialog. Disabled at 50% opacity. Visible 2px --link focus ring with 2px offset.
- Inputs: height 40px, radius 6px, 1px --line, --surface background; label above in 14/500; help/error text below in 12px.
- Panels: --surface, 1px --line, radius 8px, padding 20-24px. No nested cards.
- Tables: full width, header row in --surface-sunken with 12/500 --muted text (sentence case), rows 48px with 1px --line dividers, row hover --surface-sunken. Primary cell = name/ID (mono for IDs) + muted secondary line.
- State indicator: 8px solid dot in the state color + sentence-case label ("Running", "Starting", "Stopped", "Error"); identical everywhere.
- Empty states: one short sentence saying what to do + one primary button. No illustrations, no emoji, no glyph art.
- Icons: none, or simple 16px inline SVG strokes (1.5px) where they add meaning (e.g. copy, external link). No unicode glyphs as icons.

## Layout
- App shell: left sidebar 220px (--surface, right border) with the wordmark "boxes" (simple lowercase wordmark, 16/600, with a small square outline mark) and text nav items (40px rows, active = --surface-sunken background + --ink text + 2px left bar in --ink); account (email + sign out) at the bottom. Main area: page header row (title 20/600 left, primary action right), then content max-width 1200px, left aligned, 32px page padding.
- Auth pages (login/signup): single centered column 360px wide on --bg; wordmark, title "Sign in" / "Create the owner account", one line naming the server host, fields, full-width primary button, one text link. Nothing else.
- Responsive: sidebar collapses to a top bar under 900px; tables become stacked rows under 640px.

## Brand
- Use the square b primary mark from `web/public/brand/mark.svg` in product UI. Inline it through `BrandMark` so the mark follows `currentColor`; the bowl's top wall stays a separate element for its loading motion.
- Use the solid mark for app and browser icons. The `boxes` wordmark uses IBM Plex Sans 600 at 16px in the sidebar and 20px on auth pages.
- The lifted lid pose is reserved for first data loading and auth submission. Keep the static mark still everywhere else.

## Motion
- Use CSS motion only, with durations from 120–200ms and `cubic-bezier(.2,.8,.2,1)` easing for interface transitions. Reduced motion disables animation and transition effects.
- Dialog backdrops fade in; dialog panels fade and scale from .98 to 1. Mobile navigation fades and moves down 8px. Toasts fade and move up 8px.
- Active tabs and segmented controls use a single sliding indicator/background. Buttons move down 1px while pressed; primary buttons darken slightly on hover.
- Provisioning, resuming, starting, and stopping state dots pulse between .45 and 1 opacity over 1.2 seconds. Running, stopped, and error dots remain static.
- Initial list rows fade in once, with 20ms stagger and a 10-row limit; polling and later refreshes do not replay the entrance.

## Spacing
- Use a shared `SectionHeader` for section titles, optional descriptions, and actions. The row aligns to the top, uses a 16px gap, and leaves 16px before its content. Headings use 16px/24px at weight 600; descriptions use 14px/20px in `--muted`, start 4px below the heading, and stop at 60 characters per line. Actions use 32px buttons aligned with the heading's first line.
- Separate page sections by 40px. Leave 24px between the page header and first content. Do not wrap ordinary section content in a slab; reserve panels for detail lists, dialogs, and forms.
- Use `InlineForm` for compact row forms. Fields align at the bottom with a 12px gap; labels are 14px/500 with 6px before controls. Inputs and submit buttons are 40px tall, port/number inputs are 160px wide, and text inputs expand. Leave 16px after the form; stack fields below 640px.
- Table headers and body cells share 12px 16px padding. Empty-state rows use the same cell padding. Tables have no top margin; the preceding header or form establishes the spacing.
- Activity uses a plain usage summary, a compact 30-day inline bar chart, a segmented category filter, and a full-width table. Charts use `--ink` at 70% opacity, a baseline, and only endpoint date labels plus the maximum-hours label. Activity timestamps show relative time with the full timestamp in the `title` attribute.
- Desktop is view-only by default. A transparent input-blocking overlay sits over the frame until the user selects Take control; the control choice is session-only and is released with its button.
