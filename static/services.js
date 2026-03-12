// @ts-check

/**
 * @typedef {Object} ServiceInfo
 * @property {string} name        - Service name (without .service suffix)
 * @property {string} loadState   - e.g. "loaded", "not-found", "masked"
 * @property {string} activeState - e.g. "active", "inactive", "failed"
 * @property {string} subState    - e.g. "running", "dead", "exited", "failed"
 * @property {string} description - Human-readable description
 */

// ---- State ----

/** @type {'all'|'running'|'failed'|'inactive'} */
let currentFilter = 'all';

/** @type {string} */
let searchQuery = '';

/** @type {ServiceInfo[]} */
let lastServices = [];

// ---- Status helpers ----

/**
 * Return the dot CSS class for a service's state.
 * @param {ServiceInfo} svc
 * @returns {string}
 */
const dotClass = (svc) => {
  if (svc.activeState === 'failed') return 'crit';
  if (svc.activeState === 'active' && svc.subState === 'running') return 'ok';
  if (svc.activeState === 'active') return 'warn'; // exited, etc.
  if (svc.activeState === 'activating' || svc.activeState === 'deactivating') return 'warn';
  return 'off'; // inactive, masked, not-found
};

/**
 * Return the sub-state badge CSS class.
 * @param {ServiceInfo} svc
 * @returns {string}
 */
const subClass = (svc) => {
  if (svc.subState === 'running')  return 'running';
  if (svc.subState === 'failed' || svc.activeState === 'failed') return 'failed';
  if (svc.subState === 'exited')   return 'exited';
  return '';
};

/**
 * Sort key: failed first, then running, then exited/active, then inactive, then the rest.
 * @param {ServiceInfo} svc
 * @returns {number}
 */
const sortKey = (svc) => {
  if (svc.activeState === 'failed') return 0;
  if (svc.activeState === 'active' && svc.subState === 'running') return 1;
  if (svc.activeState === 'active') return 2;
  if (svc.activeState === 'activating' || svc.activeState === 'deactivating') return 3;
  if (svc.activeState === 'inactive') return 4;
  return 5;
};

/** @type {Record<string, string>} Maps raw systemd sub-states to display labels. */
const SUB_LABELS = {
  running:      'running',
  exited:       'completed',
  dead:         'stopped',
  failed:       'failed',
  start:        'starting',
  'start-pre':  'starting',
  'start-post': 'starting',
  stop:         'stopping',
  'stop-post':  'stopping',
  'stop-sigterm': 'stopping',
  'stop-sigkill': 'stopping',
  mounted:      'mounted',
  waiting:      'waiting',
  elapsed:      'elapsed',
  plugged:      'plugged',
};

/**
 * Return a human-readable label for a service's current state.
 * @param {ServiceInfo} svc
 * @returns {string}
 */
const subLabel = (svc) => {
  if (svc.activeState === 'failed') return 'failed';
  return SUB_LABELS[svc.subState] ?? svc.subState;
};

// ---- Log accordion ----

/** @type {string|null} Name of the currently expanded service, or null. */
let expandedUnit = null;

/**
 * Fetch and render logs for a unit into its expansion panel.
 * @param {string} unit
 * @param {HTMLElement} panel
 */
const loadLogs = async (unit, panel) => {
  panel.innerHTML = '<div class="svc-log-loading">Loading…</div>';
  try {
    const resp = await fetch(`/api/logs?unit=${encodeURIComponent(unit)}`);
    /** @type {{timestamp:string, message:string, priority:number}[]} */
    const entries = await resp.json();
    if (!Array.isArray(entries) || !entries.length) {
      panel.innerHTML = '<div class="svc-log-loading">No log entries found.</div>';
      return;
    }
    panel.innerHTML = entries.map((e) => {
      const priClass = e.priority <= 3 ? 'log-crit' : e.priority === 4 ? 'log-warn' : '';
      const msg = e.message.replace(/&/g,'&amp;').replace(/</g,'&lt;');
      return (
        `<div class="svc-log-entry">` +
          `<span class="svc-log-ts">${e.timestamp}</span>` +
          `<span class="svc-log-msg ${priClass}">${msg}</span>` +
        `</div>`
      );
    }).join('');
    // Scroll to bottom so newest entry is visible
    panel.scrollTop = panel.scrollHeight;
  } catch (e) {
    panel.innerHTML = `<div class="svc-log-loading">Error: ${e instanceof Error ? e.message : String(e)}</div>`;
  }
};

// ---- Render ----

/**
 * Apply current filter and search, then re-render the list.
 */
const renderServices = () => {
  const list = document.getElementById('svc-list');
  if (!list) return;

  if (!lastServices.length) {
    list.innerHTML = '<div class="svc-unavailable">systemd service data unavailable — is systemctl accessible?</div>';
    return;
  }

  const q = searchQuery.toLowerCase();

  const visible = lastServices
    .filter((svc) => {
      if (currentFilter === 'running' && !(svc.activeState === 'active' && svc.subState === 'running')) return false;
      if (currentFilter === 'failed'  && svc.activeState !== 'failed')  return false;
      if (currentFilter === 'inactive' && svc.activeState !== 'inactive') return false;
      if (q && !svc.name.toLowerCase().includes(q) && !svc.description.toLowerCase().includes(q)) return false;
      return true;
    })
    .sort((a, b) => sortKey(a) - sortKey(b) || a.name.localeCompare(b.name));

  if (!visible.length) {
    list.innerHTML = '<div class="svc-empty">No services match.</div>';
    // Reset expanded state since items are gone
    expandedUnit = null;
    return;
  }

  list.innerHTML = visible.map((svc) => {
    const dc  = dotClass(svc);
    const sc  = subClass(svc);
    const sub = subLabel(svc);
    const isOpen = expandedUnit === svc.name;
    return (
      `<div class="svc-item${isOpen ? ' svc-item-open' : ''}" data-unit="${svc.name}">` +
        `<div class="svc-row">` +
          `<span class="svc-dot ${dc}"></span>` +
          `<span class="svc-name">${svc.name}</span>` +
          `<span class="svc-sub ${sc}">${sub}</span>` +
          `<span class="svc-desc">${svc.description}</span>` +
          `<span class="svc-chevron">${isOpen ? '▲' : '▼'}</span>` +
        `</div>` +
        `<div class="svc-log-panel"${isOpen ? '' : ' style="display:none"'}></div>` +
      `</div>`
    );
  }).join('');

  // Re-populate the open panel after each re-render (innerHTML wipes it)
  if (expandedUnit) {
    const item = list.querySelector(`[data-unit="${expandedUnit}"]`);
    const panel = item?.querySelector('.svc-log-panel');
    if (panel instanceof HTMLElement && !panel.innerHTML) {
      loadLogs(expandedUnit, panel);
    }
  }
};

// ---- Fetch ----

const refresh = async () => {
  const list = document.getElementById('svc-list');
  try {
    const resp = await fetch('/api/services');
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
    /** @type {{services: ServiceInfo[], error?: string}} */
    const data = await resp.json();
    if (data.error) {
      if (list) list.innerHTML = `<div class="svc-unavailable">${data.error}</div>`;
      return;
    }
    lastServices = data.services ?? [];
    renderServices();
  } catch (e) {
    if (list) list.innerHTML = `<div class="svc-unavailable">Error: ${e instanceof Error ? e.message : String(e)}</div>`;
  }
};

// ---- Controls ----

const DEFAULT_RATE = 10000;

/** @type {number|null} */
let intervalId = null;

const getSavedRate = () =>
  parseInt(localStorage.getItem('svcRefreshRate') ?? String(DEFAULT_RATE), 10);

const applyRate = (/** @type {number} */ ms) => {
  localStorage.setItem('svcRefreshRate', String(ms));
  if (intervalId !== null) clearInterval(intervalId);
  intervalId = setInterval(refresh, ms);
};

// Single persistent click handler for the service list (event delegation)
document.getElementById('svc-list')?.addEventListener('click', (e) => {
  const list = document.getElementById('svc-list');
  if (!list) return;

  const target = e.target instanceof Element ? e.target : null;
  if (!target) return;

  // Ignore clicks inside the log panel (scrolling, text selection, etc.)
  if (target.closest('.svc-log-panel')) return;

  const item = /** @type {HTMLElement|null} */ (target.closest('.svc-item'));
  if (!item) return;

  const unit = item.dataset['unit'] ?? '';
  const panel = /** @type {HTMLElement|null} */ (item.querySelector('.svc-log-panel'));
  if (!panel) return;

  if (expandedUnit === unit) {
    // Collapse
    expandedUnit = null;
    panel.style.display = 'none';
    item.classList.remove('svc-item-open');
    const chev = item.querySelector('.svc-chevron');
    if (chev) chev.textContent = '▼';
  } else {
    // Collapse previous if any
    if (expandedUnit) {
      const prev = list.querySelector(`[data-unit="${expandedUnit}"]`);
      if (prev) {
        const prevPanel = prev.querySelector('.svc-log-panel');
        if (prevPanel instanceof HTMLElement) prevPanel.style.display = 'none';
        prev.classList.remove('svc-item-open');
        const prevChev = prev.querySelector('.svc-chevron');
        if (prevChev) prevChev.textContent = '▼';
      }
    }
    // Expand this one
    expandedUnit = unit;
    panel.style.display = 'block';
    item.classList.add('svc-item-open');
    const chev = item.querySelector('.svc-chevron');
    if (chev) chev.textContent = '▲';
    loadLogs(unit, panel);
  }
});

// Filter buttons
document.getElementById('svc-filter')?.addEventListener('click', (e) => {
  const btn = /** @type {HTMLElement} */ (e.target);
  const f = btn.dataset['filter'];
  if (!f) return;
  currentFilter = /** @type {typeof currentFilter} */ (f);
  document.querySelectorAll('#svc-filter .tog').forEach((b) => b.classList.remove('tog-active'));
  btn.classList.add('tog-active');
  renderServices();
});

// Search input
document.getElementById('svc-search')?.addEventListener('input', (e) => {
  searchQuery = /** @type {HTMLInputElement} */ (e.target).value;
  renderServices();
});

// Refresh rate selector
const rateSelect = /** @type {HTMLSelectElement|null} */ (document.getElementById('rate-select'));
if (rateSelect) {
  rateSelect.value = String(getSavedRate());
  rateSelect.addEventListener('change', () => applyRate(parseInt(rateSelect.value, 10)));
}

// Hostname from hardware info
fetch('/api/hardware')
  .then((r) => r.json())
  .then((data) => {
    if (data.hostname) {
      document.title = `${data.hostname} — Services`;
      const h1 = document.querySelector('header h1');
      if (h1) h1.textContent = `🖥 ${data.hostname}`;
    }
  });

refresh();
applyRate(getSavedRate());
