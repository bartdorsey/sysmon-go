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
 * Return a CSS class name based on usage percentage.
 * @param {number} p - Usage percentage (0-100)
 * @returns {'ok'|'warn'|'crit'}
 */
function lvl(p) {
  return p < 70 ? 'ok' : p < 85 ? 'warn' : 'crit';
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
 * Fetch fresh disk data from the API and re-render the grid.
 * @returns {Promise<void>}
 */
async function refresh() {
  try {
    var r = await fetch('/api/disk');
    if (!r.ok) throw new Error('HTTP ' + r.status);
    /** @type {FSInfo[]} */
    var data = await r.json();
    var grid = document.getElementById('grid');
    grid.innerHTML = data.length
      ? data.map(card).join('')
      : '<div class="msg">No filesystems found.</div>';
  } catch (e) {
    document.getElementById('grid').innerHTML = '<div class="msg">Error: ' + e.message + '</div>';
  }
}

refresh();
setInterval(refresh, 5000);
