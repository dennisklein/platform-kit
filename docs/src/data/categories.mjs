// Link categories. Each category has a URL scheme for authors (`[](kind:Host)`), a label,
// a color token (defined per theme in src/styles/theme-*.css) and a 16px stroke icon.
// scripts/xref.mjs resolves the scheme; src/components/CategoryStyles.astro makes the CSS.

/** Stroke paths on a 16x16 grid, 1.5px wide, round caps. */
const icons = {
  cube: '<path d="M8 1.75 13.5 4.8v6.4L8 14.25 2.5 11.2V4.8Z"/><path d="M2.5 4.8 8 7.9l5.5-3.1M8 7.9v6.35"/>',
  loop: '<path d="M13 6.5A5.25 5.25 0 0 0 3.4 5"/><path d="M3 2.5V5.25h2.75"/><path d="M3 9.5A5.25 5.25 0 0 0 12.6 11"/><path d="M13 13.5v-2.75h-2.75"/>',
  play: '<rect x="2" y="2" width="12" height="12" rx="2"/><path d="m6.5 5.25 4 2.75-4 2.75Z"/>',
  terminal: '<rect x="1.75" y="2.5" width="12.5" height="11" rx="1.5"/><path d="m4.5 6.25 2 1.75-2 1.75M8.25 10.25h3"/>',
  flask: '<path d="M6 1.75h4M6.5 1.75v4.5L2.6 12.6a1 1 0 0 0 .85 1.65h9.1a1 1 0 0 0 .85-1.65L9.5 6.25v-4.5"/><path d="M4.25 10h7.5"/>',
  bulb: '<path d="M6 12h4M6.5 14.25h3"/><path d="M5.6 9.8A4.5 4.5 0 1 1 10.4 9.8c-.55.45-.9 1.1-.9 1.8V12h-3v-.4c0-.7-.35-1.35-.9-1.8Z"/>',
  branch: '<circle cx="4" cy="3.5" r="1.5"/><circle cx="4" cy="12.5" r="1.5"/><circle cx="12" cy="6" r="1.5"/><path d="M4 5v6M12 7.5c0 2.5-3 2.5-6.5 4"/>',
  wrench: '<path d="M10.5 2a3.5 3.5 0 0 0-3.3 4.65L2.4 11.45a1.4 1.4 0 0 0 2 2l4.8-4.8A3.5 3.5 0 0 0 14 5.5l-2.1 2.1-2.2-.4-.4-2.2L11.4 2.9A3.5 3.5 0 0 0 10.5 2Z"/>',
  buoy: '<circle cx="8" cy="8" r="6.25"/><circle cx="8" cy="8" r="2.5"/><path d="m3.6 3.6 2.6 2.6M9.8 9.8l2.6 2.6M12.4 3.6 9.8 6.2M6.2 9.8l-2.6 2.6"/>',
  steps: '<path d="M1.75 13.5h3.5v-3h3.5v-3h3.5v-3h2"/><path d="M1.75 13.5h12.5"/>',
  table: '<rect x="2" y="2.5" width="12" height="11" rx="1.5"/><path d="M2 6.25h12M2 9.75h12M6.5 6.25v7.25"/>',
  book: '<path d="M2.25 3.25A1.5 1.5 0 0 1 3.75 1.75h9.5v10.5h-9.5a1.5 1.5 0 0 0-1.5 1.5Z"/><path d="M2.25 13.75a1.5 1.5 0 0 0 1.5 1h9.5v-2.5"/>',
  external: '<path d="M6 3H3.25v9.75H13V10"/><path d="M9 2h5v5M14 2 7.5 8.5"/>',
  chip: '<rect x="4" y="4" width="8" height="8" rx="1"/><path d="M6 1.75V4M10 1.75V4M6 12v2.25M10 12v2.25M1.75 6H4M1.75 10H4M12 6h2.25M12 10h2.25"/>',
  plug: '<path d="M5.5 1.75v3M10.5 1.75v3M3.75 4.75h8.5v2.5a4.25 4.25 0 0 1-8.5 0ZM8 11.5v2.75"/>',
  wheel: '<circle cx="8" cy="8" r="6"/><circle cx="8" cy="8" r="1.75"/><path d="M8 2v4.25M8 9.75V14M2.8 5l3.7 2.1M9.5 8.9l3.7 2.1M13.2 5 9.5 7.1M6.5 8.9 2.8 11"/>',
  layers: '<path d="M8 1.75 14.25 5 8 8.25 1.75 5Z"/><path d="m1.75 8 6.25 3.25L14.25 8M1.75 11 8 14.25 14.25 11"/>',
  shield: '<path d="M8 1.75 13.25 3.5v4.25c0 3.25-2.25 5.5-5.25 6.5-3-1-5.25-3.25-5.25-6.5V3.5Z"/><path d="m5.75 8 1.6 1.6 2.9-3.1"/>',
  calendar: '<rect x="2" y="3" width="12" height="11" rx="1.5"/><path d="M2 6.5h12M5.5 1.75v2.5M10.5 1.75v2.5M5 9.25h2M9 9.25h2M5 11.75h2"/>',
  file: '<path d="M9.25 1.75H4A1.25 1.25 0 0 0 2.75 3v10A1.25 1.25 0 0 0 4 14.25h8A1.25 1.25 0 0 0 13.25 13V5.75Z"/><path d="M9.25 1.75v4h4M6.75 8.5 5.25 10l1.5 1.5M9.25 8.5l1.5 1.5-1.5 1.5"/>',
};

/**
 * The categories, in legend order. `scheme` is what authors write before the colon,
 * `label` is the name in the legend and in hover cards, `mono` sets the link in the
 * code font, `prefix` is put before an empty link's canonical text.
 */
export const categories = [
  { id: 'kind', scheme: 'kind', label: 'Kind', icon: 'cube', mono: false, layer: 'reference' },
  { id: 'ctrl', scheme: 'ctrl', label: 'Controller', icon: 'loop', mono: false, layer: 'reference' },
  { id: 'act', scheme: 'act', label: 'Action', icon: 'play', mono: false, layer: 'reference' },
  { id: 'cli', scheme: 'cli', label: 'Command', icon: 'terminal', mono: true, layer: 'reference', prefix: 'pk-metal ' },
  { id: 'lab', scheme: 'lab', label: 'Lab', icon: 'flask', mono: true, layer: 'reference', prefix: 'mise run lab:' },
  { id: 'ref', scheme: 'ref', label: 'Reference', icon: 'table', mono: false, layer: 'reference' },
  { id: 'concept', scheme: 'concept', label: 'Concept', icon: 'bulb', mono: false, layer: 'learn' },
  { id: 'flow', scheme: 'flow', label: 'Workflow', icon: 'branch', mono: false, layer: 'learn' },
  { id: 'howto', scheme: 'howto', label: 'How-to', icon: 'wrench', mono: false, layer: 'operate' },
  { id: 'rb', scheme: 'rb', label: 'Runbook', icon: 'buoy', mono: false, layer: 'operate' },
  { id: 'tut', scheme: 'tut', label: 'Tutorial', icon: 'steps', mono: false, layer: 'learn' },
  { id: 'term', scheme: 'term', label: 'Glossary term', icon: 'book', mono: false, layer: 'reference' },
  { id: 'ext', scheme: null, label: 'External', icon: 'external', mono: false, layer: null },
  { id: 'src', scheme: null, label: 'Source file', icon: 'file', mono: true, layer: null },
];

/** @type {[string, ...string[]]} */
export const categoryIds = /** @type {any} */ (categories.map((c) => c.id));

export const byScheme = new Map(categories.filter((c) => c.scheme).map((c) => [c.scheme, c]));
export const byId = new Map(categories.map((c) => [c.id, c]));

/** The icon of a category or glossary domain as a full SVG document. */
export function iconSvg(name, { stroke = 'currentColor', size = 16 } = {}) {
  const body = icons[name];
  if (!body) throw new Error(`categories: no icon named "${name}"`);
  return `<svg xmlns="http://www.w3.org/2000/svg" width="${size}" height="${size}" viewBox="0 0 16 16" fill="none" stroke="${stroke}" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">${body}</svg>`;
}

/** The icon as a CSS url() value, for `mask`. */
export function iconUrl(name) {
  return `url("data:image/svg+xml,${encodeURIComponent(iconSvg(name, { stroke: '#000' }))}")`;
}

export { icons };
