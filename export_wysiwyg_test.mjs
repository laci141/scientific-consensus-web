// WYSIWYG export test: proves that every export format returns exactly the
// rows visible on screen (quick filter + sort applied), with a provenance
// header. Runs the real <script> from index.html inside a minimal fake DOM —
// no browser needed. Usage: node export_wysiwyg_test.mjs
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

// ── minimal DOM ──────────────────────────────────────────────────────────
function matchSimple(el, s) {
  const attrM = s.match(/\[([a-z-]+)\]$/);
  let attr = null;
  if (attrM) { attr = attrM[1]; s = s.slice(0, attrM.index); }
  let ok = true;
  if (s.startsWith('.')) ok = el._classes.includes(s.slice(1));
  else if (s) ok = el.tagName === s.toUpperCase();
  if (ok && attr) {
    const key = attr.replace(/^data-/, '').replace(/-([a-z])/g, (_, c) => c.toUpperCase());
    ok = el.dataset && el.dataset[key] !== undefined;
  }
  return ok;
}
function matchSel(el, sel) {
  return sel.split(',').some(part => {
    const chain = part.trim().split(/\s+/);
    if (!matchSimple(el, chain[chain.length - 1])) return false;
    let anc = el.parentElement, idx = chain.length - 2;
    while (idx >= 0 && anc) {
      if (matchSimple(anc, chain[idx])) idx--;
      anc = anc.parentElement;
    }
    return idx < 0;
  });
}
class El {
  constructor(tag, classes = [], opts = {}) {
    this.tagName = tag.toUpperCase();
    this._classes = classes;
    this.classList = { contains: c => this._classes.includes(c) };
    this.dataset = opts.dataset || {};
    this.style = { display: '' };
    this.value = opts.value !== undefined ? opts.value : '';
    this._text = opts.text || '';
    this.children = [];
    this.parentElement = null;
    this.isConnected = true;
    this._order = 0;
  }
  get textContent() { return this._text + this.children.map(c => c.textContent).join(''); }
  set textContent(v) { this._text = v; this.children = []; }
  add(...kids) { kids.forEach(k => { k.parentElement = this; this.children.push(k); }); return this; }
  *walk() { for (const c of this.children) { yield c; yield* c.walk(); } }
  querySelectorAll(sel) { return [...this.walk()].filter(el => matchSel(el, sel)); }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  closest(sel) { let n = this; while (n) { if (matchSel(n, sel)) return n; n = n.parentElement; } return null; }
  compareDocumentPosition(other) { return other._order > this._order ? 4 : 2; }
}
function renumber(root) { let i = 0; root._order = i++; for (const el of root.walk()) el._order = i++; }

// ── sandbox with the real page script ────────────────────────────────────
const html = readFileSync(new URL('./index.html', import.meta.url), 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];

const listeners = {};
const alerts = [];
const downloads = [];
const colName = c => String.fromCharCode(65 + c); // test data stays under 26 columns
const sandbox = {
  console,
  alerts, downloads,
  Node: { DOCUMENT_POSITION_FOLLOWING: 4 },
  alert: m => alerts.push(m),
  Blob: class { constructor(parts, opts) { this.text = parts.join(''); this.type = opts && opts.type; } },
  URL: { createObjectURL: () => 'blob:x', revokeObjectURL: () => {} },
  window: { location: { origin: 'http://localhost' }, addEventListener: () => {} },
  localStorage: { getItem: () => null, setItem: () => {} },
  document: {
    addEventListener: (t, fn) => { (listeners[t] = listeners[t] || []).push(fn); },
    querySelectorAll: () => [],
    querySelector: () => null,
    getElementById: () => null,
  },
  XLSX: {
    utils: {
      aoa_to_sheet: aoa => {
        const ws = { __aoa: aoa };
        const rows = aoa.length, cols = Math.max(...aoa.map(r => r.length), 1);
        ws['!ref'] = 'A1:' + colName(cols - 1) + rows;
        aoa.forEach((r, R) => r.forEach((v, C) => { ws[colName(C) + (R + 1)] = { v }; }));
        return ws;
      },
      decode_range: ref => {
        const m = ref.match(/^([A-Z])(\d+):([A-Z])(\d+)$/);
        return { s: { c: m[1].charCodeAt(0) - 65, r: +m[2] - 1 }, e: { c: m[3].charCodeAt(0) - 65, r: +m[4] - 1 } };
      },
      encode_cell: ({ r, c }) => colName(c) + (r + 1),
      book_new: () => ({}),
      book_append_sheet: (wb, ws) => { wb.ws = ws; },
    },
    writeFile: (wb, filename) => { downloads.push({ filename, wb }); },
  },
};
vm.createContext(sandbox);
vm.runInContext(script, sandbox);
const ctx = code => vm.runInContext(code, sandbox);

let failures = 0;
function check(name, actual, expected) {
  const ok = JSON.stringify(actual) === JSON.stringify(expected);
  if (!ok) failures++;
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}  →  ${JSON.stringify(actual)}${ok ? '' : ' (expected ' + JSON.stringify(expected) + ')'}`);
}
const fireInput = el => listeners.input.forEach(fn => fn({ target: el }));
// capture downloads instead of touching the (absent) real DOM
ctx(`triggerDownload = (blob, filename) => { downloads.push({ text: blob.text, filename }); };`);

function metaRowEl(key) {
  const row = new El('div', ['meta-row'], { dataset: { key } });
  const meta = new El('span', ['meta'], { text: 'meta' });
  meta.add(new El('span', ['qcount']));
  row.add(meta, new El('input', ['qfilter'], { value: '' }));
  return row;
}
const csvDataRows = text => text.replace(/^﻿/, '').split('\r\n').filter(Boolean).length - 3; // sep + header + cols

// ── scenario 1: consensus studies, text filter ───────────────────────────
const root1 = new El('div');
const card1 = new El('div', ['result-card']);
const mr1 = metaRowEl('consensus_studies');
const titles = Array.from({ length: 10 }, (_, i) => (i % 3 === 0 ? `magnesium study ${i}` : `placebo study ${i}`));
card1.add(mr1, ...titles.map(t => new El('div', ['study-card'], { text: t })));
root1.add(card1); renumber(root1);
sandbox.__root1 = root1;
ctx(`lastQuery = 'magnesium lowers blood pressure';
     lastData.consensus_studies = ${JSON.stringify(titles.map((t, i) => ({ title: t, year: 2020 + (i % 5), citations: i * 10, doi: '10.1/x' + i, group: i < 6 ? 'supporting' : 'refuting' })))};
     bindExportViews(__root1);`);
check('S1 initial qcount', mr1.querySelector('.qcount').textContent, ' · 10 / 10 rows');
mr1.querySelector('.qfilter').value = 'magnesium';
fireInput(mr1.querySelector('.qfilter'));
const visible1 = card1.querySelectorAll('.study-card').filter(el => el.style.display !== 'none').length;
check('S1 visible cards after filter', visible1, 4);
check('S1 qcount after filter', mr1.querySelector('.qcount').textContent, ' · 4 / 10 rows');
check('S1 exportRows == visible', ctx(`exportRows('consensus_studies').rows.length`), visible1);
ctx(`downloadCSV('consensus_studies','t.csv'); downloadXLSX('consensus_studies','t.xlsx');
     downloadBibTeX('consensus_studies','t.bib'); downloadJSON('consensus_studies','t.json');`);
const [csv1, xlsx1, bib1, json1] = downloads.slice(-4);
check('S1 CSV data rows', csvDataRows(csv1.text), 4);
check('S1 XLSX data rows', xlsx1.wb.ws.__aoa.length - 2, 4);
check('S1 BibTeX entries', (bib1.text.match(/@article\{/g) || []).length, 4);
const j1 = JSON.parse(json1.text);
check('S1 JSON rows', j1.rows.length, 4);
check('S1 JSON envelope counts', [j1.export.rows_exported, j1.export.rows_total], [4, 10]);
check('S1 JSON filters', j1.export.filters, ['filter: "magnesium"']);
check('S1 CSV provenance line', csv1.text.split('\r\n')[1].includes('showing 4 of 10 rows'), true);
check('S1 XLSX caption row', xlsx1.wb.ws.__aoa[0][0].startsWith('Corpova export'), true);
check('S1 XLSX freeze below caption+header', xlsx1.wb.ws['!freeze'].ySplit, 2);
check('S1 BibTeX provenance comment', bib1.text.startsWith('% Corpova export'), true);
check('S1 JSON app/query', [j1.export.app, j1.export.query], ['Corpova', 'magnesium lowers blood pressure']);

// ── scenario 2: evidence pyramid table, sorted view ──────────────────────
const root2 = new El('div');
const card2 = new El('div', ['result-card']);
const mr2 = metaRowEl('evidence_pyramid');
const table = new El('table');
const th = new El('th', [], { text: 'Count', dataset: {} });
const headTr = new El('tr').add(th);
const designs = ['meta-analysis', 'rct', 'cohort', 'case-report'];
const dataTrs = designs.map((d, i) => new El('tr').add(new El('td', [], { text: d }), new El('td', [], { text: String(40 - i * 10) })));
table.add(headTr, ...dataTrs);
card2.add(mr2, table); root2.add(card2); renumber(root2);
sandbox.__root2 = root2;
ctx(`lastData.evidence_pyramid = { rows: ${JSON.stringify(designs.map((d, i) => ({ design: d, count: 40 - i * 10, pct: 25 })))}, response: { ok: true } };
     bindExportViews(__root2);`);
// simulate a th click-sort: reverse the data rows in the DOM, mark the column
table.children = [headTr, ...dataTrs.slice().reverse()];
renumber(root2);
th.dataset.dir = 'asc';
check('S2 export follows sorted DOM order', ctx(`exportRows('evidence_pyramid').rows.map(r => r.design)`),
  ['case-report', 'cohort', 'rct', 'meta-analysis']);
check('S2 sort recorded in filters', ctx(`exportRows('evidence_pyramid').filters`), ['sorted by "Count" asc']);
ctx(`downloadJSON('evidence_pyramid','p.json');`);
const j2 = JSON.parse(downloads[downloads.length - 1].text);
check('S2 JSON keeps full response', j2.response.ok, true);
check('S2 JSON first row is screen-first row', j2.rows[0].design, 'case-report');

// ── scenario 3: compare — two cards share one export key ─────────────────
const root3 = new El('div');
const cardA = new El('div', ['result-card']);
const cardB = new El('div', ['result-card']);
const mrA = metaRowEl('compare_studies'), mrB = metaRowEl('compare_studies');
const aTitles = ['alpha trial 1', 'beta trial 2', 'beta trial 3', 'beta trial 4'];
const bTitles = ['gamma trial 5', 'gamma trial 6', 'gamma trial 7'];
cardA.add(mrA, ...aTitles.map(t => new El('div', ['study-card'], { text: t })));
cardB.add(mrB, ...bTitles.map(t => new El('div', ['study-card'], { text: t })));
root3.add(cardA, cardB); renumber(root3);
sandbox.__root3 = root3;
ctx(`lastData.compare_studies = ${JSON.stringify([...aTitles.map(t => ({ title: t, claim: 'A' })), ...bTitles.map(t => ({ title: t, claim: 'B' }))])};
     bindExportViews(__root3);`);
mrA.querySelector('.qfilter').value = 'alpha';
fireInput(mrA.querySelector('.qfilter'));
check('S3 claim A filtered, claim B untouched', ctx(`exportRows('compare_studies').rows.map(r => r.title)`),
  ['alpha trial 1', ...bTitles]);
check('S3 qcount A', mrA.querySelector('.qcount').textContent, ' · 1 / 4 rows');
check('S3 qcount B', mrB.querySelector('.qcount').textContent, ' · 3 / 3 rows');

// ── scenario 4: everything filtered out → alert, no file ─────────────────
mr1.querySelector('.qfilter').value = 'zzz-no-match';
fireInput(mr1.querySelector('.qfilter'));
const before = downloads.length;
ctx(`downloadCSV('consensus_studies','t.csv');`);
check('S4 empty view alert', alerts[alerts.length - 1], 'Run an analysis first — or clear the filters, nothing is visible.');
check('S4 no file downloaded', downloads.length, before);

console.log(failures ? `\n${failures} FAILURE(S)` : '\nALL CHECKS PASSED');
process.exit(failures ? 1 : 0);
