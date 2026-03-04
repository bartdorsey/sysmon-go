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
 * @typedef {Object} SystemInfo
 * @property {number}   cpuPct - CPU usage percentage (0-100)
 * @property {MemInfo}  memory - Memory info
 * @property {SwapInfo} swap   - Swap info
 */

/**
 * Format a byte count into a human-readable string.
 * @param {number} b - Bytes
 * @returns {string}
 */
function fmt(b) {
  if (b === 0) return '0 B';
  var k = 1024, s = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  var i = Math.floor(Math.log(b) / Math.log(k));
  return (b / Math.pow(k, i)).toFixed(1) + '\u00a0' + s[i];
}

/**
 * Return a CSS level class name based on usage percentage.
 * @param {number} p - Usage percentage (0-100)
 * @returns {'ok'|'warn'|'crit'}
 */
function lvl(p) {
  return p < 70 ? 'ok' : p < 85 ? 'warn' : 'crit';
}

/**
 * Build an HTML string for a system stat card (CPU, Memory, or Swap).
 * @param {string} title   - Card title
 * @param {number} pct     - Usage percentage (0-100)
 * @param {string} subline - Secondary info line (e.g. "used / total")
 * @returns {string}
 */
function sysCard(title, pct, subline) {
  var c = lvl(pct);
  return '<div class="card sys-card">' +
    '<div class="sys-label">' + title + '</div>' +
    '<div class="sys-pct ' + c + '">' + pct.toFixed(1) + '%</div>' +
    '<div class="track"><div class="fill ' + c + '" style="width:' + pct.toFixed(1) + '%"></div></div>' +
    '<div class="sys-sub">' + subline + '</div>' +
  '</div>';
}

/**
 * Render system stats (CPU, memory, swap) into the sys-grid element.
 * @param {SystemInfo} data
 */
function renderSystem(data) {
  var html = sysCard('CPU', data.cpuPct, '&nbsp;');
  html += sysCard(
    'Memory',
    data.memory.usedPct,
    fmt(data.memory.used) + ' / ' + fmt(data.memory.total)
  );
  if (data.swap.total > 0) {
    html += sysCard(
      'Swap',
      data.swap.usedPct,
      fmt(data.swap.used) + ' / ' + fmt(data.swap.total)
    );
  }
  document.getElementById('sys-grid').innerHTML = html;
}

/**
 * Build the HTML string for a single filesystem card.
 * @param {FSInfo} fs
 * @returns {string}
 */
function card(fs) {
  var c = lvl(fs.usedPct);
  var p = fs.usedPct.toFixed(1);
  return '<div class="card">' +
    '<div class="card-top">' +
      '<div>' +
        '<div class="mount">' + fs.mountPoint + '</div>' +
        '<div class="device">' + fs.device + '</div>' +
      '</div>' +
      '<span class="badge">' + fs.fsType + '</span>' +
    '</div>' +
    '<div class="track"><div class="fill ' + c + '" style="width:' + p + '%"></div></div>' +
    '<div class="nums">' +
      '<div><div class="num-label">Free</div><div class="num-value">' + fmt(fs.free) + '</div></div>' +
      '<div><div class="num-label">Used</div><div class="num-value ' + c + '">' + p + '%</div></div>' +
      '<div><div class="num-label">Total</div><div class="num-value">' + fmt(fs.total) + '</div></div>' +
    '</div>' +
  '</div>';
}

/**
 * Fetch fresh disk and system data from the API and re-render the page.
 * @returns {Promise<void>}
 */
async function refresh() {
  try {
    var [diskResp, sysResp] = await Promise.all([
      fetch('/api/disk'),
      fetch('/api/system')
    ]);
    if (!diskResp.ok) throw new Error('disk: HTTP ' + diskResp.status);
    if (!sysResp.ok) throw new Error('system: HTTP ' + sysResp.status);

    /** @type {FSInfo[]} */
    var diskData = await diskResp.json();
    /** @type {SystemInfo} */
    var sysData = await sysResp.json();

    renderSystem(sysData);

    var grid = document.getElementById('grid');
    grid.innerHTML = diskData.length
      ? diskData.map(card).join('')
      : '<div class="msg">No filesystems found.</div>';
  } catch (e) {
    document.getElementById('grid').innerHTML = '<div class="msg">Error: ' + e.message + '</div>';
  }
}

refresh();
setInterval(refresh, 5000);
