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

  // Status label -> design-token custom property for the queue doughnut. Labels
  // that are not in this map (none today) fall back to the accent color.
  var QUEUE_COLOR_VARS = {
    Pending: '--mx-chart-pending',
    Processing: '--mx-chart-processing',
    Done: '--mx-chart-done',
    Failed: '--mx-chart-failed',
    Deferred: '--mx-chart-deferred',
  };

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
      var varName = QUEUE_COLOR_VARS[label];
      return varName ? resolveVar(canvas, varName, accentColor) : accentColor;
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

  document.querySelectorAll('canvas[data-chart-type]').forEach(function (canvas) {
    var type = canvas.getAttribute('data-chart-type');
    var labels = parseAttr(canvas, 'data-chart-labels');
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
