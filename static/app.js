// @ts-check

/**
 * @typedef {Object} FSInfo
 * @property {string} mountPoint - Filesystem mount point path
 * @property {string} device     - Block device path or network share
 * @property {string} fsType     - Filesystem type (ext4, xfs, etc.)
 * @property {number} total      - Total bytes
 * @property {number} free       - Free bytes
 * @property {number} used       - Used bytes
 * @property {number} usedPct    - Used percentage (0-100)
 */

/**
 * @typedef {Object} MemInfo
 * @property {number} total     - Total bytes
 * @property {number} used      - Used bytes
 * @property {number} free      - Free bytes
 * @property {number} available - Available bytes
 * @property {number} usedPct   - Used percentage (0-100)
 */

/**
 * @typedef {Object} SwapInfo
 * @property {number} total   - Total bytes
 * @property {number} used    - Used bytes
 * @property {number} free    - Free bytes
 * @property {number} usedPct - Used percentage (0-100)
 */

/**
 * @typedef {Object} LoadAvg
 * @property {number} one     - 1-minute load average
 * @property {number} five    - 5-minute load average
 * @property {number} fifteen - 15-minute load average
 */

/**
 * @typedef {Object} SystemInfo
 * @property {number}   cpuPct      - Overall CPU usage percentage (0-100)
 * @property {number[]} cores       - Per-core usage percentages (0-100)
 * @property {MemInfo}  memory      - Memory info
 * @property {SwapInfo} swap        - Swap info
 * @property {string}   uptime      - e.g. "3d 14h 22m"
 * @property {LoadAvg}  loadAvg     - Load averages
 * @property {number}   totalProcs  - Total number of processes
 */

/**
 * @typedef {Object} NetIface
 * @property {string} name
 * @property {number} rxRate  - bytes/sec received
 * @property {number} txRate  - bytes/sec transmitted
 * @property {number} rxTotal - cumulative bytes received
 * @property {number} txTotal - cumulative bytes transmitted
 * @property {string} mac     - MAC address
 * @property {number} speed   - link speed in Mbps (0 = unknown)
 * @property {string} driver  - kernel driver name
 */

/**
 * @typedef {Object} ProcInfo
 * @property {number} pid
 * @property {string} name
 * @property {number} cpuPct
 * @property {number} memBytes
 */

/**
 * @typedef {Object} ProcessesResponse
 * @property {ProcInfo[]} byCPU
 * @property {ProcInfo[]} byMem
 */

/**
 * @typedef {Object} HardwareInfo
 * @property {string} hostname  - Host system hostname
 * @property {string} cpuModel - e.g. "Intel(R) Core(TM) i9-13900K CPU @ 3.00GHz"
 * @property {string} ramType  - e.g. "DDR4"
 * @property {string} ramSpeed - e.g. "3200 MT/s"
 * @property {string} os       - e.g. "Ubuntu 22.04.3 LTS"
 * @property {string} osId     - e.g. "ubuntu", "debian", "arch"
 * @property {string} kernel    - e.g. "6.6.114.1-microsoft-standard-WSL2"
 * @property {number} coreCount - Number of logical CPU cores
 * @property {string} arch      - e.g. "x86_64", "aarch64"
 * @property {number} cpuMaxMHz - CPU max frequency in MHz
 */

// ---- History buffers ----

/** Maximum number of data points retained per metric. */
const MAX_HISTORY = 60;

/** @type {number[]} Rolling CPU usage history (percentages). */
const cpuHistory = [];

/** @type {number[]} Rolling memory usage history (percentages). */
const memHistory = [];

/** @type {number[]} Rolling swap usage history (percentages). */
const swapHistory = [];

/** @type {Record<string, number[]>} Rolling total throughput history (bytes/sec) per interface. */
const netHistory = {};

/** @type {HardwareInfo} Cached hardware info fetched once on load. */
let hwInfo = { cpuModel: '', ramType: '', ramSpeed: '', hostname: '', os: '', osId: '', kernel: '', coreCount: 0, arch: '', cpuMaxMHz: 0 };

/**
 * Append a value to a history array, capping it at MAX_HISTORY entries.
 * @param {number[]} arr - The history array to update.
 * @param {number}   val - Value to append.
 */
const pushHistory = (arr, val) => {
  arr.push(val);
  if (arr.length > MAX_HISTORY) arr.shift();
};

// ---- Utilities ----

/**
 * Format a byte count into a human-readable string.
 * @param {number} b - Bytes
 * @returns {string}
 */
const fmt = (b) => {
  if (b === 0) return '0\u00a0B';
  const k = 1024;
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  const i = Math.floor(Math.log(b) / Math.log(k));
  return `${(b / k ** i).toFixed(1)}\u00a0${sizes[i]}`;
};

/**
 * Return a CSS level class name based on usage percentage.
 * @param {number} p - Usage percentage (0-100)
 * @returns {'ok'|'warn'|'crit'}
 */
const lvl = (p) => p < 70 ? 'ok' : p < 85 ? 'warn' : 'crit';

// ---- Graph ----

/** @type {Record<string, string>} Map of level name to hex colour. */
const PALETTE = { ok: '#3fb950', warn: '#d29922', crit: '#f85149' };

/**
 * Draw a time-series line graph on a canvas element.
 * Accounts for devicePixelRatio for crisp rendering on HiDPI displays.
 * @param {HTMLCanvasElement|null} canvas
 * @param {number[]} data          - Percentage values (0-100), oldest first.
 * @param {'ok'|'warn'|'crit'} level
 */
const drawGraph = (canvas, data, level) => {
  if (!canvas || data.length < 2) return;

  const ctx = canvas.getContext('2d');
  if (!ctx) return;

  const dpr  = window.devicePixelRatio || 1;
  const cssW = canvas.offsetWidth;
  const cssH = canvas.offsetHeight;
  canvas.width  = cssW * dpr;
  canvas.height = cssH * dpr;
  ctx.scale(dpr, dpr);

  const w     = cssW;
  const h     = cssH;
  const color = PALETTE[level] ?? PALETTE.ok;

  // Subtle horizontal grid lines at 25 / 50 / 75 %
  for (const p of [0.25, 0.5, 0.75]) {
    ctx.beginPath();
    ctx.strokeStyle = 'rgba(255,255,255,0.06)';
    ctx.lineWidth = 1;
    ctx.moveTo(0, h * (1 - p));
    ctx.lineTo(w, h * (1 - p));
    ctx.stroke();
  }

  // Pre-compute pixel coordinates.
  // Use a fixed step based on MAX_HISTORY so the newest point is always at the
  // right edge and the graph fills in from right to left as data accumulates.
  const step = w / (MAX_HISTORY - 1);
  const pts = data.map((v, i) => ({
    x: w - (data.length - 1 - i) * step,
    y: h - (Math.min(Math.max(v, 0), 100) / 100) * h,
  }));

  // Gradient fill under the line
  ctx.beginPath();
  ctx.moveTo(pts[0].x, h);
  for (const p of pts) ctx.lineTo(p.x, p.y);
  ctx.lineTo(pts[pts.length - 1].x, h);
  ctx.closePath();
  const grad = ctx.createLinearGradient(0, 0, 0, h);
  grad.addColorStop(0, `${color}55`);
  grad.addColorStop(1, `${color}08`);
  ctx.fillStyle = grad;
  ctx.fill();

  // Line
  ctx.beginPath();
  pts.forEach((p, i) => {
    if (i === 0) ctx.moveTo(p.x, p.y);
    else         ctx.lineTo(p.x, p.y);
  });
  ctx.strokeStyle = color;
  ctx.lineWidth   = 1.5;
  ctx.lineJoin    = 'round';
  ctx.stroke();
};

// ---- Neofetch bar ----

/** @type {Record<string, string>} Map of os-release ID to emoji icon. */
const DISTRO_ICONS = {
  ubuntu:       '🟠',
  debian:       '🌀',
  fedora:       '🎩',
  arch:         '🔷',
  manjaro:      '🟢',
  opensuse:     '🦎',
  'opensuse-leap': '🦎',
  'opensuse-tumbleweed': '🦎',
  alpine:       '🏔️',
  centos:       '🔴',
  rhel:         '🔴',
  rocky:        '🪨',
  almalinux:    '🔴',
  nixos:        '❄️',
  void:         '⬛',
  gentoo:       '🐉',
  kali:         '🐉',
  pop:          '🟣',
  linuxmint:    '🟢',
  raspbian:     '🍓',
};

/** @type {string} Last uptime string from stats refresh. */
let lastUptime = '';

/** @type {string} Last load average string from stats refresh. */
let lastLoadAvg = '';

/** @type {string} Last process count string from stats refresh. */
let lastTotalProcs = '';

/**
 * Render the neofetch-style info bar using cached hardware info and last uptime.
 */
const renderNeoBar = () => {
  const bar = document.getElementById('neo-bar');
  if (!bar) return;

  const distroIcon = DISTRO_ICONS[hwInfo.osId] ?? '🐧';
  const osLabel = hwInfo.os ? `${distroIcon} ${hwInfo.os}` : '';

  const freqStr = hwInfo.cpuMaxMHz >= 1000
    ? `${(hwInfo.cpuMaxMHz / 1000).toFixed(2)} GHz`
    : hwInfo.cpuMaxMHz > 0 ? `${hwInfo.cpuMaxMHz.toFixed(0)} MHz` : '';
  const cpuLabel = [hwInfo.cpuModel, hwInfo.coreCount ? `${hwInfo.coreCount} cores` : '', freqStr]
    .filter(Boolean).join(' · ');
  const ramLabel = [hwInfo.ramType, hwInfo.ramSpeed].filter(s => s && s !== 'Unknown').join(' ');

  const fields = /** @type {[string, string][]} */ ([
    ['OS',      osLabel],
    ['Kernel',  hwInfo.kernel],
    ['Arch',    hwInfo.arch],
    ['CPU',     cpuLabel],
    ['RAM',     ramLabel],
    ['Load',    lastLoadAvg],
    ['Procs',   lastTotalProcs],
    ['Uptime',  lastUptime],
  ].filter(f => f[1]));

  if (!fields.length) { bar.innerHTML = ''; return; }

  const items = fields.map(([k, v]) =>
    `<span class="neo-item"><span class="neo-key">${k}</span><span class="neo-val">${v}</span></span>`
  ).join('');

  bar.innerHTML = `<div class="neo-bar">${items}</div>`;
};

// ---- System cards ----

/**
 * Build the compact per-core usage grid HTML.
 * @param {number[]} cores - Per-core usage percentages (0-100).
 * @returns {string}
 */
const renderCores = (cores) => {
  if (!cores || cores.length === 0) return '';
  const items = cores.map((pct, i) => {
    const c = lvl(pct);
    return (
      `<div class="core-item">` +
        `<span class="core-pct">${pct.toFixed(0)}%</span>` +
        `<div class="core-track"><div class="core-fill ${c}" style="height:${Math.min(pct, 100).toFixed(1)}%"></div></div>` +
        `<span class="core-label">C${i}</span>` +
      `</div>`
    );
  }).join('');
  return `<div class="cores-grid">${items}</div>`;
};

/**
 * Build the CPU card: overall %, graph canvas, and per-core grid.
 * @param {number}   pct   - Overall CPU usage percentage (0-100).
 * @param {number[]} cores - Per-core usage percentages (0-100).
 * @returns {string}
 */
const cpuCard = (pct, cores) => {
  const c = lvl(pct);
  const label = hwInfo.cpuModel || 'CPU';
  return (
    `<div class="card sys-card sys-card-cpu">` +
      `<div class="cpu-top">` +
        `<div class="cpu-main">` +
          `<div class="sys-label">${label}</div>` +
          `<div class="sys-pct ${c}">${pct.toFixed(1)}%</div>` +
        `</div>` +
        `<canvas id="graph-cpu" class="sys-graph cpu-graph"></canvas>` +
      `</div>` +
      renderCores(cores) +
    `</div>`
  );
};

/**
 * Build an HTML string for a system stat card with an embedded graph canvas.
 * @param {string} id      - Unique canvas ID suffix (e.g. 'mem', 'swap').
 * @param {string} title   - Card title.
 * @param {number} pct     - Current usage percentage (0-100).
 * @param {string} subline - Secondary info line (e.g. "used / total").
 * @returns {string}
 */
const sysCard = (id, title, pct, subline) => {
  const c = lvl(pct);
  return (
    `<div class="card sys-card">` +
      `<div class="sys-label">${title}</div>` +
      `<div class="sys-pct ${c}">${pct.toFixed(1)}%</div>` +
      `<canvas id="graph-${id}" class="sys-graph"></canvas>` +
      `<div class="sys-sub">${subline}</div>` +
    `</div>`
  );
};

/**
 * Render system stats (CPU, memory, swap) into the sys-grid element,
 * then draw the history graphs on each card's canvas.
 * @param {SystemInfo} data
 */
const renderSystem = (data) => {
  pushHistory(cpuHistory,  data.cpuPct);
  pushHistory(memHistory,  data.memory.usedPct);
  pushHistory(swapHistory, data.swap.usedPct);

  // CPU card lives outside sys-grid so it doesn't constrain the grid column count.
  const cpuContainer = document.getElementById('cpu-card');
  if (cpuContainer) cpuContainer.innerHTML = cpuCard(data.cpuPct, data.cores);

  const ramTag = hwInfo.ramType && hwInfo.ramType !== 'Unknown'
    ? [hwInfo.ramType, hwInfo.ramSpeed].filter(Boolean).join(' ')
    : '';
  const memTitle = ramTag || 'Memory';
  let memHtml = sysCard('mem', memTitle, data.memory.usedPct,
    `${fmt(data.memory.used)} / ${fmt(data.memory.total)}`);
  if (data.swap.total > 0) {
    memHtml += sysCard('swap', 'Swap', data.swap.usedPct,
      `${fmt(data.swap.used)} / ${fmt(data.swap.total)}`);
  }

  // Set innerHTML first so canvas elements exist in the DOM before drawing.
  const sysGrid = document.getElementById('sys-grid');
  if (sysGrid) sysGrid.innerHTML = memHtml;

  /** @param {string} id @returns {HTMLCanvasElement|null} */
  const getCanvas = (id) => /** @type {HTMLCanvasElement|null} */ (document.getElementById(id));

  drawGraph(getCanvas('graph-cpu'), cpuHistory,  lvl(data.cpuPct));
  drawGraph(getCanvas('graph-mem'), memHistory,  lvl(data.memory.usedPct));
  if (data.swap.total > 0) {
    drawGraph(getCanvas('graph-swap'), swapHistory, lvl(data.swap.usedPct));
  }
};

// ---- Filesystem cards ----

/**
 * Build the HTML string for a single filesystem card.
 * @param {FSInfo} fs
 * @returns {string}
 */
const card = (fs) => {
  const c = lvl(fs.usedPct);
  const p = fs.usedPct.toFixed(1);
  return (
    `<div class="card">` +
      `<div class="card-top">` +
        `<div>` +
          `<div class="mount">${fs.mountPoint}</div>` +
          `<div class="device">${fs.device}</div>` +
        `</div>` +
        `<span class="badge">${fs.fsType}</span>` +
      `</div>` +
      `<div class="track"><div class="fill ${c}" style="width:${p}%"></div></div>` +
      `<div class="nums">` +
        `<div><div class="num-label">Free</div><div class="num-value">${fmt(fs.free)}</div></div>` +
        `<div><div class="num-label">Used</div><div class="num-value ${c}">${p}%</div></div>` +
        `<div><div class="num-label">Total</div><div class="num-value">${fmt(fs.total)}</div></div>` +
      `</div>` +
    `</div>`
  );
};

// ---- Process panel ----

/** @type {'cpu'|'mem'} */
let procMode = 'cpu';

/** @type {ProcessesResponse|null} */
let lastProcData = null;

/**
 * Render the process panel into #proc-card.
 * @param {ProcessesResponse} data
 */
const renderProcs = (data) => {
  lastProcData = data;
  const container = document.getElementById('proc-card');
  if (!container) return;

  const procs = procMode === 'cpu' ? data.byCPU : data.byMem;
  const topVal = procs.length
    ? (procMode === 'cpu' ? procs[0].cpuPct : procs[0].memBytes)
    : 1;

  const rows = procs.map((p) => {
    const raw  = procMode === 'cpu' ? p.cpuPct : p.memBytes;
    const bar  = topVal > 0 ? (raw / topVal) * 100 : 0;
    const disp = procMode === 'cpu' ? `${p.cpuPct.toFixed(1)}%` : fmt(p.memBytes);
    const c    = procMode === 'cpu' ? lvl(p.cpuPct) : 'ok';
    return (
      `<div class="proc-row">` +
        `<span class="proc-name">${p.name}</span>` +
        `<div class="proc-track"><div class="proc-fill ${c}" style="width:${bar.toFixed(1)}%"></div></div>` +
        `<span class="proc-val">${disp}</span>` +
      `</div>`
    );
  }).join('');

  container.innerHTML =
    `<div class="card" style="height:100%;box-sizing:border-box">` +
      `<div class="proc-header">` +
        `<span class="proc-title">Processes</span>` +
        `<div class="proc-toggle">` +
          `<button class="tog${procMode === 'cpu' ? ' tog-active' : ''}" id="tog-cpu">CPU</button>` +
          `<button class="tog${procMode === 'mem' ? ' tog-active' : ''}" id="tog-mem">MEM</button>` +
        `</div>` +
      `</div>` +
      `<div class="proc-list">${rows}</div>` +
    `</div>`;

  document.getElementById('tog-cpu')?.addEventListener('click', () => {
    procMode = 'cpu';
    if (lastProcData) renderProcs(lastProcData);
  });
  document.getElementById('tog-mem')?.addEventListener('click', () => {
    procMode = 'mem';
    if (lastProcData) renderProcs(lastProcData);
  });
};

// ---- Network cards ----

/**
 * Format a bytes/sec rate as a human-readable string.
 * @param {number} bps
 * @returns {string}
 */
const fmtRate = (bps) => `${fmt(bps)}/s`;

/**
 * Draw a network throughput graph, normalising values against the peak in the history.
 * @param {HTMLCanvasElement|null} canvas
 * @param {number[]} data - Absolute bytes/sec values, oldest first.
 */
const drawNetGraph = (canvas, data) => {
  const peak = Math.max(...data, 1);
  drawGraph(canvas, data.map(v => (v / peak) * 100), 'ok');
};

/**
 * Build a network interface card.
 * @param {NetIface} iface
 * @returns {string}
 */
const netCard = (iface) => {
  if (!netHistory[iface.name]) netHistory[iface.name] = [];
  pushHistory(netHistory[iface.name], iface.rxRate + iface.txRate);
  const speedStr = iface.speed >= 1000
    ? `${iface.speed / 1000} GbE`
    : iface.speed > 0 ? `${iface.speed} Mbps` : '';
  const metaParts = [iface.driver, speedStr].filter(Boolean);
  const metaLine = metaParts.length
    ? `<div class="net-meta">${metaParts.join(' · ')}</div>`
    : '';
  const macLine = iface.mac
    ? `<div class="net-mac">${iface.mac}</div>`
    : '';
  return (
    `<div class="card sys-card">` +
      `<div class="sys-label">${iface.name}</div>` +
      metaLine +
      macLine +
      `<canvas id="graph-net-${iface.name}" class="sys-graph"></canvas>` +
      `<div class="net-rates">` +
        `<div><span class="net-dir">&#8595;</span> ${fmtRate(iface.rxRate)}</div>` +
        `<div><span class="net-dir">&#8593;</span> ${fmtRate(iface.txRate)}</div>` +
      `</div>` +
    `</div>`
  );
};

/**
 * Render network interfaces into #net-grid.
 * @param {NetIface[]} data
 */
const renderNetwork = (data) => {
  const grid = document.getElementById('net-grid');
  if (!grid) return;
  if (!data.length) {
    grid.innerHTML = '<div class="msg">No network interfaces found.</div>';
    return;
  }
  grid.innerHTML = data.map(netCard).join('');
  for (const iface of data) {
    const canvas = /** @type {HTMLCanvasElement|null} */ (document.getElementById(`graph-net-${iface.name}`));
    if (canvas && netHistory[iface.name]) drawNetGraph(canvas, netHistory[iface.name]);
  }
};

// ---- Refresh loop ----

/**
 * Fetch fresh disk and system data from the API and re-render the page.
 * @returns {Promise<void>}
 */
const refresh = async () => {
  try {
    const resp = await fetch('/api/stats');
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`);

    const data = await resp.json();
    /** @type {SystemInfo} */
    const sysData  = data.system;
    /** @type {FSInfo[]} */
    const diskData = data.disk;
    /** @type {ProcessesResponse} */
    const procData = data.processes;
    /** @type {NetIface[]} */
    const netData  = data.network;

    lastUptime = sysData.uptime || '';
    if (sysData.loadAvg) {
      const l = sysData.loadAvg;
      lastLoadAvg = `${l.one.toFixed(2)} ${l.five.toFixed(2)} ${l.fifteen.toFixed(2)}`;
    }
    lastTotalProcs = sysData.totalProcs ? String(sysData.totalProcs) : '';
    renderNeoBar();
    renderSystem(sysData);
    renderProcs(procData);
    renderNetwork(netData);

    const grid = document.getElementById('grid');
    if (grid) {
      grid.innerHTML = diskData.length
        ? diskData.map(card).join('')
        : '<div class="msg">No filesystems found.</div>';
    }
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e);
    const grid = document.getElementById('grid');
    if (grid) grid.innerHTML = `<div class="msg">Error: ${msg}</div>`;
  }
};

// ---- Refresh rate ----

/** Default refresh interval in milliseconds. */
const DEFAULT_RATE = 5000;

/** @type {number|null} Active interval timer ID. */
let intervalId = null;

/**
 * Read the saved refresh rate from localStorage.
 * @returns {number} Milliseconds between refreshes.
 */
const getSavedRate = () =>
  parseInt(localStorage.getItem('refreshRate') ?? String(DEFAULT_RATE), 10);

/**
 * Apply a new refresh rate: persist it, clear the old timer, start a new one.
 * @param {number} ms - Milliseconds between refreshes.
 */
const applyRate = (ms) => {
  localStorage.setItem('refreshRate', String(ms));
  if (intervalId !== null) clearInterval(intervalId);
  intervalId = setInterval(refresh, ms);
};

const rateSelect = /** @type {HTMLSelectElement|null} */ (document.getElementById('rate-select'));
if (rateSelect) {
  rateSelect.value = String(getSavedRate());
  rateSelect.addEventListener('change', () => applyRate(parseInt(rateSelect.value, 10)));
}

fetch('/api/hardware')
  .then((r) => r.json())
  .then((/** @type {HardwareInfo} */ data) => {
    hwInfo = data;
    if (data.hostname) {
      document.title = `${data.hostname} — System Monitor`;
      const h1 = document.querySelector('header h1');
      if (h1) h1.textContent = `🖥 ${data.hostname}`;
    }
    renderNeoBar();
  });

refresh();
applyRate(getSavedRate());
