/* pi-token-usage — client
 *
 * The whole cube ships in one bootstrap payload (~50 KB gzipped), so every filter
 * and aggregate below runs in the browser. No request is made for a filter
 * change; the only network calls are the initial load and an explicit re-index.
 *
 * Primary metric is NEW tokens (input + output). cacheRead is deliberately
 * secondary: it is ~95% of raw volume and would swamp the signal.
 */
'use strict';

const C = {
  input:  '#2ee6f0',
  output: '#ffc45c',
  total:  '#a78bfa',
  cache:  '#5d6f89',
  green:  '#4ade9b',
  pink:   '#ff7ab8',
  blue:   '#60a5fa',
  grid:   'rgba(255,255,255,0.05)',
  tick:   '#7f92ab',
};

const SERIES = [
  '#2ee6f0', '#a78bfa', '#ffc45c', '#4ade9b',
  '#ff7ab8', '#60a5fa', '#f97316', '#c084fc',
];

Chart.defaults.color = C.tick;
Chart.defaults.font.family = 'ui-monospace, "JetBrains Mono", Menlo, Consolas, monospace';
Chart.defaults.font.size = 11;
Chart.defaults.borderColor = C.grid;
Chart.defaults.animation.duration = 500;

let DATA = null;
const charts = {};

const $ = (id) => document.getElementById(id);

const state = {
  project: '', model: '', kind: '', source: '',
  gran: 'month', from: '', to: '',
  showCache: false, log: false,
};

/* ---------------- formatting ---------------- */

function fmtTok(n) {
  n = Number(n) || 0;
  const a = Math.abs(n);
  if (a >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (a >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (a >= 1e3) return (n / 1e3).toFixed(1) + 'K';
  return String(Math.round(n));
}
function fmtInt(n) { return Math.round(Number(n) || 0).toLocaleString('en-US'); }
function fmtPct(n) { return (Number(n) || 0).toFixed(1) + '%'; }

function weekStart(day) {
  const [y, m, d] = day.split('-').map(Number);
  const dt = new Date(Date.UTC(y, (m || 1) - 1, d || 1));
  const wd = (dt.getUTCDay() + 6) % 7;          // Monday = 0
  dt.setUTCDate(dt.getUTCDate() - wd);
  return dt.toISOString().slice(0, 10);
}

function periodKey(day, gran) {
  if (gran === 'day') return day;
  if (gran === 'week') return weekStart(day);
  return day.slice(0, 7);
}

const MON = ['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'];
function prettyPeriod(k, gran) {
  if (gran === 'month') {
    const [y, m] = k.split('-');
    return MON[Number(m) - 1] + ' ' + y;
  }
  return k;
}

/* ---------------- filtering / aggregation ---------------- */

function filtered() {
  if (!DATA || !DATA.cube) return [];
  return DATA.cube.filter((r) =>
    (!state.project || r.p === state.project) &&
    (!state.model || r.m === state.model) &&
    (!state.kind || r.k === state.kind) &&
    (!state.source || r.s === state.source) &&
    (!state.from || r.d >= state.from) &&
    (!state.to || r.d <= state.to)
  );
}

function sumBy(rows, keyFn) {
  const out = new Map();
  for (const r of rows) {
    const k = keyFn(r);
    let a = out.get(k);
    if (!a) { a = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, reasoning: 0, total: 0, msgs: 0 }; out.set(k, a); }
    a.input += r.i; a.output += r.o; a.cacheRead += r.c; a.cacheWrite += r.w;
    a.reasoning += r.r; a.total += r.t; a.msgs += r.n;
  }
  return out;
}

// The headline number: fresh work, excluding cache re-reads.
const freshOf = (a) => a.input + a.output;

/* ---------------- hero ---------------- */

function renderHero(rows) {
  const t = { input: 0, output: 0, cacheRead: 0, total: 0, msgs: 0 };
  const days = new Set();
  const projs = new Set();

  for (const r of rows) {
    t.input += r.i; t.output += r.o; t.cacheRead += r.c; t.total += r.t; t.msgs += r.n;
    if (r.d) days.add(r.d);
    if (r.p) projs.add(r.p);
  }

  const fresh = t.input + t.output;
  const activeDays = days.size || 1;

  countUp($('hNew'), fresh, fmtTok);
  countUp($('hIn'), t.input, fmtTok);
  countUp($('hOut'), t.output, fmtTok);
  countUp($('hPerDay'), fresh / activeDays, fmtTok);
  countUp($('hMsgs'), t.msgs, fmtInt);
  countUp($('hProj'), projs.size, fmtInt);
  countUp($('hDays'), days.size, fmtInt);
  countUp($('hCache'), t.cacheRead, fmtTok);

  const denom = t.input + t.cacheRead;
  $('hCacheSub').textContent = denom > 0
    ? fmtPct(100 * t.cacheRead / denom) + ' of prompt volume'
    : '—';
}

function countUp(node, target, fmt) {
  if (!node) return;
  const from = Number(node.dataset.v || 0);
  node.dataset.v = target;
  if (from === target) { node.textContent = fmt(target); return; }

  const dur = 650;
  const t0 = performance.now();
  let raf = 0;
  const step = (now) => {
    const p = Math.min(1, (now - t0) / dur);
    const e = 1 - Math.pow(1 - p, 3);
    node.textContent = fmt(from + (target - from) * e);
    if (p < 1) raf = requestAnimationFrame(step);
  };
  raf = requestAnimationFrame(step);

  // rAF is throttled or never fires in background tabs and headless renders;
  // without this the number can be left showing a wrong interpolated value.
  setTimeout(() => { cancelAnimationFrame(raf); node.textContent = fmt(target); }, dur + 140);
}

/* ---------------- main throughput chart ---------------- */

function renderMain(rows) {
  const agg = sumBy(rows, (r) => periodKey(r.d, state.gran));
  const keys = [...agg.keys()].sort();
  const labels = keys.map((k) => prettyPeriod(k, state.gran));

  const ds = [
    {
      label: 'new total',
      data: keys.map((k) => freshOf(agg.get(k))),
      borderColor: C.total,
      backgroundColor: grad('mainChart', 'rgba(167,139,250,0.28)', 'rgba(167,139,250,0.01)'),
      fill: true, borderWidth: 2.2, tension: 0.3, pointRadius: 0, pointHoverRadius: 4,
    },
    {
      label: 'input',
      data: keys.map((k) => agg.get(k).input),
      borderColor: C.input, backgroundColor: 'transparent',
      fill: false, borderWidth: 2, tension: 0.3, pointRadius: 0, pointHoverRadius: 4,
    },
    {
      label: 'output',
      data: keys.map((k) => agg.get(k).output),
      borderColor: C.output, backgroundColor: 'transparent',
      fill: false, borderWidth: 2, tension: 0.3, pointRadius: 0, pointHoverRadius: 4,
    },
  ];

  if (state.showCache) {
    ds.push({
      label: 'cache re-read (secondary)',
      data: keys.map((k) => agg.get(k).cacheRead),
      borderColor: C.cache, backgroundColor: 'transparent',
      borderDash: [6, 5], borderWidth: 1.4, fill: false, tension: 0.3,
      pointRadius: 0, pointHoverRadius: 3,
    });
  }

  const yType = state.log ? 'logarithmic' : 'linear';
  const cfg = {
    type: 'line',
    data: { labels, datasets: ds },
    options: {
      responsive: true, maintainAspectRatio: false,
      interaction: { mode: 'index', intersect: false },
      plugins: {
        legend: { labels: { boxWidth: 14, boxHeight: 3, usePointStyle: false, padding: 16 } },
        tooltip: { callbacks: { label: (c) => ' ' + c.dataset.label + ': ' + fmtInt(c.parsed.y) } },
      },
      scales: {
        x: { grid: { display: false }, ticks: { maxRotation: 0, autoSkipPadding: 20 } },
        y: {
          type: yType, beginAtZero: !state.log,
          grid: { color: C.grid },
          ticks: { callback: (v) => fmtTok(v) },
        },
      },
    },
  };

  if (charts.main) {
    charts.main.data = cfg.data;
    charts.main.options.scales.y.type = yType;
    charts.main.options.scales.y.beginAtZero = !state.log;
    charts.main.update();
  } else {
    charts.main = new Chart($('mainChart'), cfg);
  }
}

// Vertical gradient fill for area charts.
function grad(canvasId, top, bottom) {
  const cv = $(canvasId);
  if (!cv || !cv.getContext) return top;
  const ctx = cv.getContext('2d');
  const g = ctx.createLinearGradient(0, 0, 0, cv.clientHeight || 300);
  g.addColorStop(0, top);
  g.addColorStop(1, bottom);
  return g;
}

/* ---------------- stacked area by project ---------------- */

function renderStack(rows) {
  const byProj = sumBy(rows, (r) => r.p);
  const ranked = [...byProj.entries()]
    .map(([p, a]) => [p, freshOf(a)])
    .sort((a, b) => b[1] - a[1]);

  const TOP = 7;
  const top = ranked.slice(0, TOP);
  const rest = ranked.slice(TOP);

  const periods = [...new Set(rows.map((r) => periodKey(r.d, state.gran)))].sort();
  const idx = new Map(periods.map((p, i) => [p, i]));

  // fresh tokens per (project, period)
  const cell = new Map();
  const other = new Array(periods.length).fill(0);
  const topNames = new Set(top.map((t) => t[0]));

  for (const r of rows) {
    const fresh = r.i + r.o;
    const pi = idx.get(periodKey(r.d, state.gran));
    if (pi === undefined) continue;
    if (topNames.has(r.p)) {
      const k = r.p + '|' + pi;
      cell.set(k, (cell.get(k) || 0) + fresh);
    } else {
      other[pi] += fresh;
    }
  }

  const ds = top.map(([name], i) => ({
    label: name,
    data: periods.map((_, pi) => cell.get(name + '|' + pi) || 0),
    backgroundColor: hexA(SERIES[i % SERIES.length], 0.55),
    borderColor: hexA(SERIES[i % SERIES.length], 0.95),
    borderWidth: 1, fill: true, tension: 0.25, pointRadius: 0, pointHoverRadius: 3,
  }));

  if (rest.length) {
    ds.push({
      label: 'other (' + rest.length + ')',
      data: other,
      backgroundColor: 'rgba(93,111,137,0.35)',
      borderColor: 'rgba(93,111,137,0.8)',
      borderWidth: 1, fill: true, tension: 0.25, pointRadius: 0, pointHoverRadius: 3,
    });
  }

  const cfg = {
    type: 'line',
    data: { labels: periods.map((p) => prettyPeriod(p, state.gran)), datasets: ds },
    options: {
      responsive: true, maintainAspectRatio: false,
      interaction: { mode: 'index', intersect: false },
      plugins: {
        legend: { labels: { boxWidth: 11, boxHeight: 11, usePointStyle: true, pointStyle: 'rectRounded', padding: 12 } },
        tooltip: { callbacks: { label: (c) => ' ' + c.dataset.label + ': ' + fmtInt(c.parsed.y) } },
      },
      scales: {
        x: { stacked: true, grid: { display: false }, ticks: { maxRotation: 0, autoSkipPadding: 20 } },
        y: { stacked: true, grid: { color: C.grid }, ticks: { callback: (v) => fmtTok(v) } },
      },
    },
  };

  if (charts.stack) { charts.stack.data = cfg.data; charts.stack.update(); }
  else charts.stack = new Chart($('stackChart'), cfg);
}

function hexA(hex, a) {
  const h = hex.replace('#', '');
  const n = parseInt(h.length === 3 ? h.split('').map((c) => c + c).join('') : h, 16);
  return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${a})`;
}

/* ---------------- hour of day ---------------- */

function renderHours(rows) {
  const buckets = new Array(24).fill(0);
  for (const r of rows) {
    const h = r.h | 0;
    if (h >= 0 && h < 24) buckets[h] += r.i + r.o;
  }
  const cfg = {
    type: 'bar',
    data: {
      labels: buckets.map((_, i) => String(i).padStart(2, '0')),
      datasets: [{
        label: 'new tokens',
        data: buckets,
        backgroundColor: buckets.map((_, i) =>
          (i >= 0 && i < 6) ? 'rgba(167,139,250,0.65)' : 'rgba(46,230,240,0.6)'),
        borderRadius: 5,
        borderSkipped: false,
      }],
    },
    options: {
      responsive: true, maintainAspectRatio: false,
      plugins: {
        legend: { display: false },
        tooltip: { callbacks: { label: (c) => ' ' + fmtInt(c.parsed.y) } },
      },
      scales: {
        x: { grid: { display: false } },
        y: { grid: { color: C.grid }, ticks: { callback: (v) => fmtTok(v) } },
      },
    },
  };
  if (charts.hour) { charts.hour.data = cfg.data; charts.hour.update(); }
  else charts.hour = new Chart($('hourChart'), cfg);
}

/* ---------------- horizontal bars ---------------- */

function renderBars(el, map, color) {
  if (!el) return;
  const items = [...map.entries()].map(([k, a]) => [k, freshOf(a)]).sort((a, b) => b[1] - a[1]).slice(0, 14);
  const max = items.length ? items[0][1] : 1;
  if (!items.length) { el.innerHTML = '<div class="empty">no data for this filter</div>'; return; }
  el.innerHTML = items.map(([name, val]) => `
    <div class="bar-row">
      <div class="bar-label">
        <span class="nm" title="${esc(name)}">${esc(name || '(unknown)')}</span>
        <span class="vl">${fmtTok(val)}</span>
      </div>
      <div class="bar-track"><div class="bar-fill" style="width:${(100 * val / max).toFixed(2)}%;background:${color}"></div></div>
    </div>`).join('');
}

function esc(s) {
  return String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
}

/* ---------------- heatmap ---------------- */

function renderHeatmap(rows) {
  const byDay = new Map();
  for (const r of rows) {
    const fresh = r.i + r.o;
    if (fresh > 0) byDay.set(r.d, (byDay.get(r.d) || 0) + fresh);
  }

  const cv = $('heatmap');
  const wrap = cv.parentElement;
  const tip = $('heatTip');

  const days = [...byDay.keys()].filter((d) => /^\d{4}-\d{2}-\d{2}$/.test(d)).sort();
  const ctx = cv.getContext('2d');

  if (!days.length) {
    cv.width = 10; cv.height = 10;
    tip.hidden = true;
    if (!wrap.querySelector('.empty')) {
      const d = document.createElement('div');
      d.className = 'empty';
      d.textContent = 'no new tokens in this range';
      wrap.appendChild(d);
    }
    return;
  }
  const empty = wrap.querySelector('.empty');
  if (empty) empty.remove();

  const cell = 15, gap = 4, left = 34, top = 22;
  const start = new Date(days[0] + 'T00:00:00Z');
  const startOff = (start.getUTCDay() + 6) % 7;
  const weeks = Math.floor((days.length + startOff + 6) / 7);

  cv.width = left + weeks * (cell + gap) + 10;
  cv.height = top + 7 * (cell + gap) + 8;

  ctx.clearRect(0, 0, cv.width, cv.height);

  const vals = [...byDay.values()];
  const maxLog = Math.log(Math.max(...vals) + 1);

  ctx.font = '9.5px ui-monospace, monospace';
  ctx.fillStyle = '#5d6f89';
  const wd = ['Mon', '', 'Wed', '', 'Fri', '', 'Sun'];
  for (let r = 0; r < 7; r++) {
    if (wd[r]) ctx.fillText(wd[r], 0, top + r * (cell + gap) + cell - 3);
  }

  const cells = [];
  for (let w = 0; w < weeks; w++) {
    for (let r = 0; r < 7; r++) {
      const x = left + w * (cell + gap);
      const y = top + r * (cell + gap);
      const d = new Date(start.getTime() + ((w * 7 + r) - startOff) * 86400000);
      const key = d.toISOString().slice(0, 10);
      const v = byDay.get(key) || 0;

      ctx.beginPath();
      ctx.roundRect(x, y, cell, cell, 3.5);
      if (v > 0) {
        const t = Math.log(v + 1) / maxLog;
        // cyan -> violet as intensity rises
        const c1 = [46, 230, 240], c2 = [167, 139, 250];
        const mix = c1.map((a, i) => Math.round(a + (c2[i] - a) * t));
        ctx.fillStyle = `rgba(${mix[0]}, ${mix[1]}, ${mix[2]}, ${(0.22 + 0.78 * t).toFixed(3)})`;
      } else {
        ctx.fillStyle = 'rgba(255,255,255,0.03)';
      }
      ctx.fill();
      cells.push({ x, y, key, v });
    }
  }

  ctx.fillStyle = '#5d6f89';
  let lastMonth = -1;
  for (let w = 0; w < weeks; w++) {
    const d = new Date(start.getTime() + (w * 7 - startOff) * 86400000);
    if (d.getUTCMonth() !== lastMonth) {
      lastMonth = d.getUTCMonth();
      ctx.fillText(MON[lastMonth], left + w * (cell + gap), 12);
    }
  }

  cv.onmousemove = (e) => {
    const rect = cv.getBoundingClientRect();
    const mx = e.clientX - rect.left, my = e.clientY - rect.top;
    const c = cells.find((c) => mx >= c.x && mx <= c.x + cell && my >= c.y && my <= c.y + cell);
    if (!c) { tip.hidden = true; return; }
    tip.hidden = false;
    tip.textContent = c.key + '  ' + (c.v > 0 ? fmtInt(c.v) + ' new tok' : 'no activity');
    tip.style.left = Math.min(c.x + cell + 10, Math.max(0, rect.width - 180)) + 'px';
    tip.style.top = (c.y + 16) + 'px';
  };
  cv.onmouseleave = () => { tip.hidden = true; };
}

/* ---------------- render all ---------------- */

function render() {
  const rows = filtered();
  renderHero(rows);
  renderMain(rows);
  renderStack(rows);
  renderBars($('projBars'), sumBy(rows, (r) => r.p), 'linear-gradient(90deg,#2ee6f0,#0891b2)');
  renderBars($('modelBars'), sumBy(rows, (r) => r.m || '(unknown)'), 'linear-gradient(90deg,#a78bfa,#7c3aed)');
  renderHeatmap(rows);
  renderHours(rows);
  writeURL();
}

/* ---------------- url state ---------------- */

function writeURL() {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(state)) if (v !== '' && v !== false) p.set(k, v);
  const q = p.toString();
  history.replaceState(null, '', q ? '#' + q : location.pathname);
}

function readURL() {
  const p = new URLSearchParams(location.hash.slice(1));
  for (const k of Object.keys(state)) {
    if (!p.has(k)) continue;
    const v = p.get(k);
    state[k] = (typeof state[k] === 'boolean') ? (v === 'true') : v;
  }
}

/* ---------------- boot ---------------- */

function fillSelect(id, values, allLabel) {
  const sel = $(id);
  if (!sel) return;
  const cur = sel.value;
  sel.innerHTML = `<option value="">${allLabel}</option>` +
    values.map((v) => `<option value="${esc(v)}">${esc(v)}</option>`).join('');
  sel.value = cur;
}

function syncControls() {
  $('fProject').value = state.project;
  $('fModel').value = state.model;
  $('fKind').value = state.kind;
  $('fSource').value = state.source;
  $('fGran').value = state.gran;
  $('fFrom').value = state.from;
  $('fTo').value = state.to;
  $('fCache').checked = !!state.showCache;
  $('fLog').checked = !!state.log;
}

function bind() {
  const on = (id, ev, fn) => { const el = $(id); if (el) el.addEventListener(ev, fn); };

  on('fProject', 'change', (e) => { state.project = e.target.value; render(); });
  on('fModel', 'change', (e) => { state.model = e.target.value; render(); });
  on('fKind', 'change', (e) => { state.kind = e.target.value; render(); });
  on('fSource', 'change', (e) => { state.source = e.target.value; render(); });
  on('fGran', 'change', (e) => { state.gran = e.target.value; render(); });
  on('fFrom', 'change', (e) => { state.from = e.target.value; render(); });
  on('fTo', 'change', (e) => { state.to = e.target.value; render(); });
  on('fCache', 'change', (e) => { state.showCache = e.target.checked; render(); });
  on('fLog', 'change', (e) => { state.log = e.target.checked; render(); });

  on('btnClear', 'click', () => {
    Object.assign(state, { project: '', model: '', kind: '', source: '', gran: 'month', from: '', to: '', showCache: false, log: false });
    syncControls(); render();
  });

  on('btnReindex', 'click', () => doIndex(false));
  on('btnFull', 'click', () => {
    if (confirm('Full rebuild clears stored offsets and re-parses every session file. Continue?')) doIndex(true);
  });

  document.addEventListener('keydown', (e) => {
    if (e.target.matches('input, select, textarea')) return;
    if (e.key === 'r') doIndex(false);
  });
}

async function doIndex(full) {
  const btn = full ? $('btnFull') : $('btnReindex');
  const other = full ? $('btnReindex') : $('btnFull');
  btn.disabled = other.disabled = true;
  $('freshness').textContent = full ? 'full rebuild running…' : 're-indexing…';
  try {
    const r = await fetch('/api/index' + (full ? '?full=true' : ''), { method: 'POST' });
    if (!r.ok) throw new Error((await r.text()).trim() || ('HTTP ' + r.status));
    await boot(true);
  } catch (err) {
    $('freshness').textContent = 'index failed: ' + err.message;
  } finally {
    btn.disabled = other.disabled = false;
  }
}

async function boot(preserveFilters) {
  const r = await fetch('/api/bootstrap');
  if (!r.ok) throw new Error('bootstrap failed: HTTP ' + r.status);
  DATA = await r.json();

  if (!preserveFilters) readURL();

  fillSelect('fProject', DATA.facets.projects || [], 'all');
  fillSelect('fModel', DATA.facets.models || [], 'all');
  fillSelect('fKind', DATA.facets.kinds || [], 'all');
  fillSelect('fSource', DATA.facets.sources || [], 'all');
  syncControls();

  const idx = DATA.index || {};
  const f = $('freshness');
  f.textContent = `${fmtInt(idx.files_total)} files · ${fmtInt(idx.events_total)} events · ` +
    (idx.refreshed ? `re-indexed in ${idx.refresh_ms} ms` : 'up to date');
  f.classList.add('live');

  const t = DATA.totals || {};
  $('footStats').textContent =
    `${fmtInt(t.total_tokens)} raw tokens · ${fmtInt(t.msgs)} counted messages · ` +
    `${t.projects} projects · ${t.days} active days`;

  render();
}

bind();
boot(false).catch((e) => { $('freshness').textContent = 'load failed: ' + e.message; });
