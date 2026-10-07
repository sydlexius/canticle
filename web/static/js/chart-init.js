// Dashboard charts (#318). Reads each <canvas data-chart-*> element produced by
// the dashChart templ component and draws a Chart.js chart. Vendored Chart.js
// (chart.umd.min.js) loads first; both files are served from /static, so they
// satisfy the serve-mode CSP's `script-src 'self'` with no inline chart code and
// no CDN. Colors are resolved from the dark design tokens (CSS custom properties)
// so a token swap re-themes the charts with no JS change.
//
// Failure is loud, never silent: a missing Chart global, an unparseable data
// attribute, or an unknown chart type logs console.error and skips that chart.
// The charts only complement the stat tiles, so a skipped chart never hides data.
// Per-provider hit-rate mini-bars (#318). Each .mx-dash-tile-bar-fill carries a
// data-hit-rate integer percent; we set the element width here from the CSSOM
// rather than via an inline style="" attribute, which the serve-mode CSP
// (style-src 'self', no unsafe-inline) would block. This runs independently of
// Chart.js so the bars render even if the vendored chart bundle failed to load.
(function () {
  'use strict';
  document.querySelectorAll('.mx-dash-tile-bar-fill[data-hit-rate]').forEach(function (fill) {
    var raw = fill.getAttribute('data-hit-rate');
    var pct = parseInt(raw, 10);
    if (isNaN(pct)) {
      console.error('dashboard hit-rate bars: bad data-hit-rate "' + raw + '"; bar skipped');
      return;
    }
    if (pct < 0) pct = 0;
    if (pct > 100) pct = 100;
    fill.style.width = pct + '%';
  });
})();

(function () {
  'use strict';

  if (typeof Chart === 'undefined') {
    console.error('dashboard charts: Chart.js global is undefined; charts disabled (vendored chart.umd.min.js failed to load?)');
    return;
  }

  // Label -> design-token custom property: the queue doughnut statuses and the
  // source page's result types. Labels not in this map (upstream names) take a
  // stable hashed fallback color instead.
  var QUEUE_COLOR_VARS = {
    Retrying: '--mx-chart-deferred',
    Errored: '--mx-chart-failed',
    Queued: '--mx-chart-pending',
    Finished: '--mx-chart-finished',
    'Settled (upgradable)': '--mx-chart-settled',
    'Given up': '--mx-chart-unavailable',
    // Source page by-type doughnut (#1300): the Results tile labels.
    'Word-synced': '--mx-chart-finished',
    'Line-synced': '--mx-chart-settled',
    Unsynced: '--mx-chart-deferred',
    Instrumental: '--mx-chart-unavailable',
    'Tier unknown': '--mx-chart-pending',
    Other: '--mx-chart-processing',
    'Not recorded': '--mx-chart-pending',
  };

  // Unmapped labels (upstream names, #1300) hash onto these, so a label keeps its
  // color when counts reorder; colors may repeat past the palette size.
  var FALLBACK_COLOR_VARS = ['--mx-chart-processing', '--mx-chart-finished', '--mx-chart-settled',
    '--mx-chart-deferred', '--mx-chart-unavailable', '--mx-chart-failed'];

  // hashLabel is a small stable string hash (djb2) for fallback color choice.
  function hashLabel(s) {
    var h = 5381;
    for (var i = 0; i < s.length; i++) h = ((h * 33) + s.charCodeAt(i)) >>> 0;
    return h;
  }

  // resolveVar reads a CSS custom property off an element, trimmed. Returns the
  // fallback (and logs) when the property is unset, so a missing token surfaces
  // rather than rendering an invisible/transparent series.
  function resolveVar(el, name, fallback) {
    var v = getComputedStyle(el).getPropertyValue(name).trim();
    if (!v) {
      console.error('dashboard charts: design token ' + name + ' is unset; using fallback');
      return fallback;
    }
    return v;
  }

  // parseAttr JSON-parses a data attribute, logging and returning null on failure.
  function parseAttr(canvas, name) {
    var raw = canvas.getAttribute(name);
    try {
      return JSON.parse(raw);
    } catch (e) {
      console.error('dashboard charts: bad ' + name + ' on #' + canvas.id + ': ' + raw, e);
      return null;
    }
  }

  // Shared dark-theme defaults pulled from the content tokens so axis labels and
  // gridlines read correctly on the navy surface.
  var probe = document.querySelector('.mx-dash-page') || document.body;
  var textColor = resolveVar(probe, '--mx-chart-text', '#94a3b8');
  var accentColor = resolveVar(probe, '--mx-accent', '#3b82f6');

  Chart.defaults.color = textColor;
  Chart.defaults.font.family = "'Inter', sans-serif";
  Chart.defaults.maintainAspectRatio = false;

  function renderDoughnut(canvas, labels, values) {
    var colors = labels.map(function (label) {
      var varName = QUEUE_COLOR_VARS[label] || FALLBACK_COLOR_VARS[hashLabel(String(label)) % FALLBACK_COLOR_VARS.length];
      return resolveVar(canvas, varName, accentColor);
    });
    return new Chart(canvas, {
      type: 'doughnut',
      data: {
        labels: labels,
        datasets: [{
          data: values,
          backgroundColor: colors,
          borderColor: resolveVar(canvas, '--mx-surface-bg', '#0f172a'),
          borderWidth: 2,
        }],
      },
      options: {
        cutout: '62%',
        plugins: {
          legend: { position: 'right', labels: { boxWidth: 12, padding: 12 } },
        },
      },
    });
  }

  // Fill patterns (solid, stripes, dots, crosshatch) so stacked series differ
  // by texture as well as color. Drawn on a small canvas; no inline style.
  function makePattern(color, index, bg) {
    if (index === 0) return color;
    var tile = document.createElement('canvas');
    tile.width = tile.height = 8;
    var g = tile.getContext('2d');
    g.fillStyle = bg;
    g.fillRect(0, 0, 8, 8);
    g.fillStyle = color;
    g.strokeStyle = color;
    g.lineWidth = 2;
    if (index === 1 || index === 3) {
      g.beginPath(); g.moveTo(0, 8); g.lineTo(8, 0); g.moveTo(-2, 2); g.lineTo(2, -2); g.moveTo(6, 10); g.lineTo(10, 6); g.stroke();
    }
    if (index === 3) {
      g.beginPath(); g.moveTo(0, 0); g.lineTo(8, 8); g.moveTo(-2, 6); g.lineTo(2, 10); g.moveTo(6, -2); g.lineTo(10, 2); g.stroke();
    }
    if (index === 2) {
      g.beginPath(); g.arc(4, 4, 2, 0, Math.PI * 2); g.fill();
    }
    return g.createPattern(tile, 'repeat');
  }

  // renderTime draws the multi-series UTC-day charts (#1302). A null point is a
  // gap: spanGaps stays false so a no-attempt day breaks the line.
  function renderTime(canvas, type, labels, series, suffix) {
    var grid = resolveVar(canvas, '--mx-chart-grid', 'rgba(255,255,255,0.08)');
    var bg = resolveVar(canvas, '--mx-surface-bg', '#0f172a');
    var stacked = type === 'stacked-bar';
    var datasets = series.map(function (se, i) {
      var varName = QUEUE_COLOR_VARS[se.label] || '--mx-chart-processing';
      var color = resolveVar(canvas, varName, accentColor);
      if (stacked) {
        return { label: se.label, data: se.data, backgroundColor: makePattern(color, i, bg), borderColor: color, borderWidth: 1 };
      }
      return { label: se.label, data: se.data, borderColor: color, backgroundColor: color, borderWidth: 2,
        pointStyle: ['circle', 'rectRot', 'triangle', 'rect'][i % 4], pointRadius: 4, borderDash: [[], [6, 3], [2, 3], [8, 3, 2, 3]][i % 4],
        spanGaps: false };
    });
    var yScale = { beginAtZero: true, grid: { color: grid }, stacked: stacked, ticks: { precision: 0 } };
    if (!stacked) { yScale.max = 100; yScale.ticks.callback = function (v) { return v + suffix; }; }
    return new Chart(canvas, {
      type: stacked ? 'bar' : 'line',
      data: { labels: labels, datasets: datasets },
      options: {
        scales: { x: { stacked: stacked, grid: { color: grid }, ticks: { autoSkip: true, maxTicksLimit: 6, maxRotation: 0 } }, y: yScale },
        plugins: { legend: { position: 'bottom', labels: { boxWidth: 14, padding: 12 } } },
      },
    });
  }

  document.querySelectorAll('canvas[data-chart-type]').forEach(function (canvas) {
    var type = canvas.getAttribute('data-chart-type');
    var labels = parseAttr(canvas, 'data-chart-labels');
    if (type === 'line' || type === 'stacked-bar') {
      var series = parseAttr(canvas, 'data-chart-series');
      var ok = Array.isArray(labels) && Array.isArray(series) && series.every(function (se) {
        return se && Array.isArray(se.data) && se.data.length === labels.length;
      });
      if (!ok) {
        console.error('dashboard charts: #' + canvas.id + ' missing/invalid labels or series; skipped');
        return;
      }
      try {
        renderTime(canvas, type, labels, series, canvas.getAttribute('data-chart-suffix') || '');
      } catch (e) {
        console.error('dashboard charts: failed to render #' + canvas.id, e);
      }
      return;
    }
    var values = parseAttr(canvas, 'data-chart-values');
    if (!Array.isArray(labels) || !Array.isArray(values) || labels.length !== values.length) {
      console.error('dashboard charts: #' + canvas.id + ' missing/invalid labels or values (length mismatch); skipped');
      return;
    }
    try {
      if (type === 'doughnut') {
        renderDoughnut(canvas, labels, values);
      } else {
        console.error('dashboard charts: unknown chart type "' + type + '" on #' + canvas.id + '; skipped');
      }
    } catch (e) {
      console.error('dashboard charts: failed to render #' + canvas.id, e);
    }
  });
})();
