/* pi-token-usage — client
 *
 * The whole cube ships in one bootstrap payload (~1-3 KB gzipped), so every
 * filter/aggregate below runs in the browser. No request is made for a filter
 * change; the only network calls are the initial load and an explicit re-index.
 */
'use strict';

const C = {
  input:  '#22d3ee',
  output: '#fbbf24',
  total:  '#a78bfa',
  cache:  '#64748b',
  green:  '#34d399',
  pink:   '#f472b6',
  grid:   'rgba(255,255,255,0.055)',
  tick:   '#7d8ea8',
};

Chart.defaults.color = C.tick;
Chart.defaults.font.family = 'ui-monospace, "JetBrains Mono", Menlo, Consolas, monospace';
Chart.defaults.font.size = 11;
Chart.defaults.borderColor = C.grid;
Chart.defaults.animation.duration = 450;

let DATA = null;
const charts = {};

const $ = (id) => document.getElementById(id);

const state = {
  project: '', model: '', kind: '',
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
  return day.slice(0, 7);                          // month
}

function prettyPeriod(k, gran) {
  if (gran === 'month') {
    const [y, m] = k.split('-');
    return ['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'][Number(m) - 1] + ' ' + y;
  }
  return k;
}

/* ---------------- filtering ---------------- */

function filtered() {
  if (!DATA || !DATA.cube) return [];
  return DATA.cube.filter((r) =>
    (!state.project || r.p === state.project) &&
    (!state.model || r.m === state.model) &&
    (!state.kind || r.k === state.kind) &&
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

/* ---------------- hero ---------------- */

function renderHero(rows) {
  const t = { input: 0, output: 0, cacheRead: 0, total: 0, msgs: 0 };
  const days = new Set();
  const projs = new Set();
  for (const r of rows) {
    t.input += r.i; t.output += r.o; t.cacheRead += r.c; t.total += r.t; t.msgs += r.n;
    days.add(r.d); projs.add(r.p);
  }
  const denom = t.input + t.cacheRead;
  const hit = denom > 0 ? (100 * t.cacheRead / denom) : 0;

  const cards = [
    { k: 'Total tokens', v: t.total, fmt: fmtTok, accent: '#a78bfa', s: fmtInt(t.total) },
    { k: 'Fresh input', v: t.input, fmt: fmtTok, accent: C.input, s: 'not served from cache' },
    { k: 'Output', v: t.output, fmt: fmtTok, accent: C.output, s: 'generated' },
    { k: 'Cache read', v: t.cacheRead, fmt: fmtTok, accent: C.cache, s: 're-read per turn' },
    { k: 'Cache hit', v: hit, fmt: fmtPct, accent: C.green, s: 'cacheRead ÷ (cacheRead+input)' },
    { k: 'Active days', v: days.size, fmt: fmtInt, accent: C.pink, s: projs.size + ' project(s)' },
  ];

  const el = $('hero');
  if (el.childElementCount !== cards.length) {
    el.innerHTML = cards.map(() =>
      '<div class="stat"><div class="k"></div><div class="v">0</div><div class="s"></div></div>').join('');
  }
  [...el.children].forEach((c, i) => {
    const cd = cards[i];
    c.style.setProperty('--accent', cd.accent);
    c.querySelector('.k').textContent = cd.k;
    c.querySelector('.s').textContent = cd.s;
    countUp(c.querySelector('.v'), cd.v, cd.fmt);
  });

  const share = t.total > 0 ? (100 * t.cacheRead / t.total) : 0;
  $('cacheShare').textContent = share.toFixed(1) + '%';
}

function countUp(node, target, fmt) {
  const from = Number(node.dataset.v || 0);
  node.dataset.v = target;
  if (from === target) { node.textContent = fmt(target); return; }

  const dur = 600;
  const t0 = performance.now();
  let raf = 0;
  const step = (now) => {
    const p = Math.min(1, (now - t0) / dur);
    const e = 1 - Math.pow(1 - p, 3);
    node.textContent = fmt(from + (target - from) * e);
    if (p < 1) raf = requestAnimationFrame(step);
  };
  raf = requestAnimationFrame(step);

  // Safety net: requestAnimationFrame is throttled or never fires in background
  // tabs and headless renders. Without this the number can be left displaying an
  // interpolated value that is simply wrong.
  setTimeout(() => {
    cancelAnimationFrame(raf);
    node.textContent = fmt(target);
  }, dur + 120);
}

/* ---------------- main throughput chart ---------------- */

function renderMain(rows) {
  const agg = sumBy(rows, (r) => periodKey(r.d, state.gran));
  const keys = [...agg.keys()].sort();
  const labels = keys.map((k) => prettyPeriod(k, state.gran));

  const ds = [
    { label: 'Total', data: keys.map((k) => agg.get(k).total), borderColor: C.total, backgroundColor: 'rgba(167,139,250,0.10)', fill: true, borderWidth: 2, tension: 0.25, pointRadius: 2 },
    { label: 'Input', data: keys.map((k) => agg.get(k).input), borderColor: C.input, backgroundColor: 'rgba(34,211,238,0.08)', fill: false, borderWidth: 2, tension: 0.25, pointRadius: 2 },
    { label: 'Output', data: keys.map((k) => agg.get(k).output), borderColor: C.output, backgroundColor: 'rgba(251,191,36,0.08)', fill: false, borderWidth: 2, tension: 0.25, pointRadius: 2 },
  ];
  if (state.showCache) {
    ds.push({
      label: 'Cache read', data: keys.map((k) => agg.get(k).cacheRead),
      borderColor: C.cache, borderDash: [5, 4], borderWidth: 1.5, fill: false, tension: 0.25, pointRadius: 1.5,
    });
  }

  const cfg = {
    type: 'line',
    data: { labels, datasets: ds },
    options: {
      responsive: true, maintainAspectRatio: false,
      interaction: { mode: 'index', intersect: false },
      plugins: {
        legend: { labels: { boxWidth: 12, boxHeight: 2, usePointStyle: false } },
        tooltip: {
          callbacks: { label: (c) => ' ' + c.dataset.label + ': ' + fmtInt(c.parsed.y) + ' tok' },
        },
      },
      scales: {
        x: { grid: { color: C.grid }, ticks: { maxRotation: 0, autoSkipPadding: 18 } },
        y: {
          type: state.log ? 'logarithmic' : 'linear',
          beginAtZero: !state.log,
          grid: { color: C.grid },
          ticks: { callback: (v) => fmtTok(v) },
        },
      },
    },
  };

  if (charts.main) {
    charts.main.data = cfg.data;
    charts.main.options.scales.y.type = cfg.options.scales.y.type;
    charts.main.options.scales.y.beginAtZero = cfg.options.scales.y.beginAtZero;
    charts.main.update();
  } else {
    charts.main = new Chart($('mainChart'), cfg);
  }
}

/* ---------------- cache gauge ---------------- */

function renderGauge(rows) {
  let cache = 0, input = 0;
  for (const r of rows) { cache += r.c; input += r.i; }
  const denom = cache + input;
  const hit = denom > 0 ? (100 * cache / denom) : 0;

  const cfg = {
    type: 'doughnut',
    data: {
      labels: ['Cache read', 'Fresh input'],
      datasets: [{
        data: [cache, input],
        backgroundColor: [C.green, C.input],
        borderColor: ['#0a0e14', '#0a0e14'],
        borderWidth: 2,
      }],
    },
    options: {
      responsive: true, maintainAspectRatio: false, cutout: '72%',
      rotation: -120, circumference: 240,
      plugins: {
        legend: { display: false },
        tooltip: { callbacks: { label: (c) => ' ' + c.label + ': ' + fmtInt(c.parsed) } },
      },
    },
  };
  if (charts.gauge) { charts.gauge.data = cfg.data; charts.gauge.update(); }
  else charts.gauge = new Chart($('gauge'), cfg);

  const cv = $('gauge');
  cv.parentElement.setAttribute('title', 'cache hit ' + hit.toFixed(2) + '%');
}

/* ---------------- hour of day ---------------- */

function renderHours(rows) {
  const buckets = new Array(24).fill(0);
  for (const r of rows) {
    const h = r.h | 0;
    if (h >= 0 && h < 24) buckets[h] += r.t;
  }
  const cfg = {
    type: 'bar',
    data: {
      labels: buckets.map((_, i) => String(i).padStart(2, '0')),
      datasets: [{
        label: 'Tokens', data: buckets,
        backgroundColor: buckets.map((v, i) => (i >= 0 && i < 6) ? 'rgba(167,139,250,0.55)' : 'rgba(34,211,238,0.55)'),
        borderRadius: 3,
      }],
    },
    options: {
      responsive: true, maintainAspectRatio: false,
      plugins: {
        legend: { display: false },
        tooltip: { callbacks: { label: (c) => ' ' + fmtInt(c.parsed.y) + ' tok' } },
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
  const items = [...map.entries()].sort((a, b) => b[1] - a[1]).slice(0, 14);
  const max = items.length ? items[0][1] : 1;
  el.innerHTML = items.map(([name, val]) => `
    <div class="bar-row">
      <div class="bar-label"><span class="nm" title="${esc(name)}">${esc(name)}</span><span class="vl">${fmtTok(val)}</span></div>
      <div class="bar-track"><div class="bar-fill" style="width:${(100 * val / max).toFixed(1)}%;background:${color}"></div></div>
    </div>`).join('') || '<div class="hint">no data for this filter</div>';
}

function esc(s) {
  return String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
}

/* ---------------- heatmap ---------------- */

function renderHeatmap(rows) {
  const byDay = new Map();
  for (const r of rows) byDay.set(r.d, (byDay.get(r.d) || 0) + r.t);

  const days = [...byDay.keys()].filter((d) => /^\d{4}-\d{2}-\d{2}$/.test(d)).sort();
  const cv = $('heatmap');
  const wrap = cv.parentElement;
  if (!days.length) {
    const ctx = cv.getContext('2d');
    ctx.clearRect(0, 0, cv.width, cv.height);
    wrap.innerHTML = '<div class="hint">no data in this range</div>';
    return;
  }

  const cell = 13, gap = 3, left = 30, top = 18;
  const start = new Date(days[0] + 'T00:00:00Z');
  const end = new Date(days[days.length - 1] + 'T00:00:00Z');
  const startOff = (start.getUTCDay() + 6) % 7;
  const weeks = Math.floor((days.length + startOff + 6) / 7);

  cv.width = left + weeks * (cell + gap) + 8;
  cv.height = top + 7 * (cell + gap) + 6;

  const ctx = cv.getContext('2d');
  ctx.clearRect(0, 0, cv.width, cv.height);

  const vals = [...byDay.values()].filter((v) => v > 0);
  const maxLog = vals.length ? Math.log(Math.max(...vals) + 1) : 1;

  const labels = ['Mon', '', 'Wed', '', 'Fri', '', 'Sun'];
  ctx.font = '9px ui-monospace, monospace';
  ctx.fillStyle = '#55657d';
  for (let r = 0; r < 7; r++) {
    if (labels[r]) ctx.fillText(labels[r], 0, top + r * (cell + gap) + cell - 3);
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
      ctx.roundRect(x, y, cell, cell, 2.5);
      if (v > 0) {
        const t = Math.log(v + 1) / maxLog;
        const a = 0.16 + 0.84 * t;
        ctx.fillStyle = `rgba(34, 211, 238, ${a.toFixed(3)})`;
      } else {
        ctx.fillStyle = 'rgba(255,255,255,0.035)';
      }
      ctx.fill();
      cells.push({ x, y, key, v });
    }
  }

  // month markers
  ctx.fillStyle = '#55657d';
  let lastMonth = -1;
  for (let w = 0; w < weeks; w++) {
    const d = new Date(start.getTime() + (w * 7 - startOff) * 86400000);
    if (d.getUTCMonth() !== lastMonth) {
      lastMonth = d.getUTCMonth();
      ctx.fillText(
        ['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'][lastMonth],
        left + w * (cell + gap), 10
      );
    }
  }

  const tip = $('heatTip');
  cv.onmousemove = (e) => {
    const rect = cv.getBoundingClientRect();
    const mx = e.clientX - rect.left, my = e.clientY - rect.top;
    const c = cells.find((c) => mx >= c.x && mx <= c.x + cell && my >= c.y && my <= c.y + cell);
    if (!c) { tip.hidden = true; return; }
    tip.hidden = false;
    tip.textContent = `${c.key}  ${c.v > 0 ? fmtInt(c.v) + ' tok' : 'no activity'}`;
    tip.style.left = Math.min(c.x + cell + 8, rect.width - 170) + 'px';
    tip.style.top = (c.y + 14) + 'px';
  };
  cv.onmouseleave = () => { tip.hidden = true; };
}

/* ---------------- sessions table ---------------- */

function renderSessions(rows) {
  const keys = new Set(rows.map((r) => r.SessionID || r.session_id));
  const ids = new Set(rows.map((r) => r.p + '|' + r.k));
  const sess = (DATA.sessions || []).filter((s) =>
    (!state.project || s.project === state.project) &&
    (!state.kind || s.kind === state.kind)
  );
  const keep = new Set();
  for (const r of rows) {
    if (state.model && r.m !== state.model) continue;
    keep.add(r.p + '|' + r.k);
  }
  const list = sess.filter((s) => keep.has(s.project + '|' + s.kind)).slice(0, 300);

  const tb = $('sessTable').querySelector('tbody');
  tb.innerHTML = list.map((s) => `
    <tr>
      <td>${esc(s.project)}</td>
      <td><span class="tag ${esc(s.kind)}">${esc(s.kind)}</span></td>
      <td>${esc(s.agent || '—')}</td>
      <td>${esc((s.started || '').replace('T', ' ').slice(0, 16))}</td>
      <td class="num">${fmtInt(s.msgs)}</td>
      <td class="num">${fmtTok(s.total)}</td>
    </tr>`).join('');
  $('sessNote').textContent = `${list.length} session(s) · ${ids.size} project/type groups`;
}

/* ---------------- render all ---------------- */

function render() {
  const rows = filtered();
  renderHero(rows);
  renderMain(rows);
  renderGauge(rows);
  renderHeatmap(rows);
  renderBars($('projBars'), sumBy(rows, (r) => r.p), 'linear-gradient(90deg,#22d3ee,#0891b2)');
  renderBars($('modelBars'), sumBy(rows, (r) => r.m || '(unknown)'), 'linear-gradient(90deg,#a78bfa,#7c3aed)');
  renderHours(rows);
  renderSessions(rows);
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
  const cur = sel.value;
  sel.innerHTML = `<option value="">${allLabel}</option>` +
    values.map((v) => `<option value="${esc(v)}">${esc(v)}</option>`).join('');
  sel.value = cur;
}

function syncControls() {
  $('fProject').value = state.project;
  $('fModel').value = state.model;
  $('fKind').value = state.kind;
  $('fGran').value = state.gran;
  $('fFrom').value = state.from;
  $('fTo').value = state.to;
  $('fCache').checked = !!state.showCache;
  $('fLog').checked = !!state.log;
}

function bind() {
  const on = (id, ev, fn) => $(id).addEventListener(ev, fn);

  on('fProject', 'change', (e) => { state.project = e.target.value; render(); });
  on('fModel', 'change', (e) => { state.model = e.target.value; render(); });
  on('fKind', 'change', (e) => { state.kind = e.target.value; render(); });
  on('fGran', 'change', (e) => { state.gran = e.target.value; render(); });
  on('fFrom', 'change', (e) => { state.from = e.target.value; render(); });
  on('fTo', 'change', (e) => { state.to = e.target.value; render(); });
  on('fCache', 'change', (e) => { state.showCache = e.target.checked; render(); });
  on('fLog', 'change', (e) => { state.log = e.target.checked; render(); });

  on('btnClear', 'click', () => {
    Object.assign(state, { project: '', model: '', kind: '', gran: 'month', from: '', to: '', showCache: false, log: false });
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
    if (!r.ok) throw new Error((await r.text()).trim() || r.status);
    await boot(true);
  } catch (err) {
    $('freshness').textContent = 'index failed: ' + err.message;
  } finally {
    btn.disabled = other.disabled = false;
  }
}

async function boot(preserveFilters) {
  const r = await fetch('/api/bootstrap');
  if (!r.ok) throw new Error('bootstrap failed: ' + r.status);
  DATA = await r.json();

  if (!preserveFilters) { readURL(); }

  fillSelect('fProject', DATA.facets.projects || [], 'All projects');
  fillSelect('fModel', DATA.facets.models || [], 'All models');
  fillSelect('fKind', DATA.facets.kinds || [], 'All types');
  syncControls();

  const idx = DATA.index || {};
  const f = $('freshness');
  f.textContent = `${fmtInt(idx.files_total)} files · ${fmtInt(idx.events_total)} events · ` +
    (idx.refreshed ? `re-indexed in ${idx.refresh_ms} ms` : 'up to date') +
    (idx.last_index ? ` · last index ${idx.last_index.replace('T', ' ').slice(0, 19)}Z` : '');
  f.classList.add('live');

  $('srcPath').textContent = (idx.sessions_root || '~/.pi/agent/sessions');
  const t = DATA.totals || {};
  $('footStats').textContent = `${fmtInt(t.total_tokens)} tokens · ${fmtInt(t.msgs)} counted messages · ` +
    `${t.projects} projects · ${t.days} active days · cache hit ${fmtPct(t.cache_hit)}`;

  render();
}

bind();
boot(false).catch((e) => { $('freshness').textContent = 'load failed: ' + e.message; });
