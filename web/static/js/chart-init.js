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

  // THE one label -> token map (queue statuses, the source page's result types,
  // the known upstream names). Doughnut slices, stacked bars and both legends all
  // resolve a category through catColor(), so one category is one solid color
  // everywhere. Labels not in the map (other upstream names) hash onto
  // FALLBACK_COLOR_VARS, which holds no semantic (result-type or error) color.
  var CAT_VARS = {
    Retrying: '--mx-chart-deferred',
    Errored: '--mx-chart-failed',
    Queued: '--mx-chart-pending',
    Finished: '--mx-chart-finished',
    'Settled (upgradable)': '--mx-chart-settled',
    'Given up': '--mx-chart-unavailable',
    'Word-synced': '--mx-chart-finished',
    'Line-synced': '--mx-chart-settled',
    Unsynced: '--mx-chart-deferred',
    Instrumental: '--mx-chart-unavailable',
    'Tier unknown': '--mx-chart-pending',
    Other: '--mx-chart-processing',
    'Not recorded': '--mx-chart-pending',
    lyricfind: '--mx-chart-up-lyricfind',
    musixmatch: '--mx-chart-up-musixmatch',
  };
  var FALLBACK_COLOR_VARS = ['--mx-chart-up-a', '--mx-chart-up-b', '--mx-chart-up-c'];

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
  var inkColor = resolveVar(probe, '--mx-chart-ink', '#f1f5f9');
  var accentColor = resolveVar(probe, '--mx-accent', '#3b82f6');
  var surfaceBg = resolveVar(probe, '--mx-surface-bg', '#0f172a');
  var gridColor = resolveVar(probe, '--mx-chart-grid', 'rgba(255,255,255,0.08)');
  var hitColor = resolveVar(probe, '--mx-chart-hit', accentColor);
  var reduceMotion = !!(window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches);

  Chart.defaults.color = textColor;
  Chart.defaults.font.family = "'Inter', sans-serif";
  Chart.defaults.maintainAspectRatio = false;
  var anim = reduceMotion ? false : { duration: 1100, easing: 'easeOutCubic' };

  // catColor: the single resolver for a category's color.
  function catColor(label) {
    var v = CAT_VARS[label] || FALLBACK_COLOR_VARS[hashLabel(String(label)) % FALLBACK_COLOR_VARS.length];
    return resolveVar(probe, v, accentColor);
  }

  function rgb(hex) {
    var h = hex.replace('#', '');
    if (h.length === 3) h = h[0] + h[0] + h[1] + h[1] + h[2] + h[2];
    return [parseInt(h.slice(0, 2), 16), parseInt(h.slice(2, 4), 16), parseInt(h.slice(4, 6), 16)];
  }
  function rgba(hex, a) { var c = rgb(hex); return 'rgba(' + c[0] + ',' + c[1] + ',' + c[2] + ',' + a + ')'; }
  // mix moves hex toward a target color by t (0..1).
  function mix(hex, target, t) {
    var a = rgb(hex), b = rgb(target);
    return 'rgb(' + a.map(function (v, i) { return Math.round(v + (b[i] - v) * t); }).join(',') + ')';
  }

  // The hit-rate line is shaded by value: weak (rose) -> mid (amber) -> strong (hit).
  var WEAK = catColor('Errored'), MID = catColor('Unsynced'), STRONG = hitColor;
  function rampColor(v) {
    var t = Math.max(0, Math.min(100, v)) / 100;
    return t < 0.5 ? mix(WEAK, MID, t * 2) : mix(MID, STRONG, (t - 0.5) * 2);
  }

  // legendLabels is THE legend component for every chart: a 12px rounded swatch
  // filled with the series' solid color (colors[] is indexed by slice or dataset),
  // never a pattern, gradient or line sample, so a legend swatch always equals the
  // mark it names.
  function legendLabels(colors, doughnut) {
    return {
      boxWidth: 12, boxHeight: 12, padding: 12, useBorderRadius: true, borderRadius: 4,
      generateLabels: function (chart) {
        var gen = doughnut ? Chart.overrides.doughnut.plugins.legend.labels.generateLabels : Chart.defaults.plugins.legend.labels.generateLabels;
        var items = gen(chart);
        items.forEach(function (it) {
          var c = colors[doughnut ? it.index : it.datasetIndex];
          // The doughnut generator sets no borderRadius on its items, so set it
          // here or doughnut swatches render square beside the rounded bar ones.
          it.fillStyle = c; it.strokeStyle = c; it.lineWidth = 0; it.borderRadius = 4;
        });
        return items;
      },
    };
  }

  function tooltipBase(callbacks) {
    return {
      backgroundColor: 'rgba(8,13,28,0.96)', borderColor: 'rgba(255,255,255,0.14)', borderWidth: 1,
      titleColor: '#e2e8f0', bodyColor: '#cbd5e1', padding: 10, cornerRadius: 8, boxPadding: 4,
      titleFont: { weight: '600' }, callbacks: callbacks,
    };
  }

  // Center total inside the doughnut hole.
  var centerPlugin = {
    id: 'mxCenter',
    afterDatasetsDraw: function (chart) {
      var meta = chart.getDatasetMeta(0);
      if (!meta.data.length) return;
      var total = chart.data.datasets[0].data.reduce(function (a, b) { return a + b; }, 0);
      var el = meta.data[0], c = chart.ctx;
      c.save();
      c.textAlign = 'center'; c.textBaseline = 'middle';
      c.fillStyle = inkColor;
      c.font = '700 ' + Math.max(18, Math.round(el.innerRadius * 0.55)) + "px 'Inter', sans-serif";
      c.fillText(String(total), el.x, el.y - el.innerRadius * 0.12);
      c.fillStyle = textColor;
      c.font = '500 ' + Math.max(10, Math.round(el.innerRadius * 0.2)) + "px 'Inter', sans-serif";
      c.fillText('TOTAL', el.x, el.y + el.innerRadius * 0.38);
      c.restore();
    },
  };

  function renderDoughnut(canvas, labels, values) {
    var colors = labels.map(catColor);
    return new Chart(canvas, {
      type: 'doughnut',
      plugins: [centerPlugin],
      data: {
        labels: labels,
        datasets: [{ data: values, backgroundColor: colors, borderWidth: 0, borderRadius: 12, spacing: 4, hoverOffset: 8 }],
      },
      options: {
        cutout: '74%', animation: anim,
        plugins: {
          legend: { position: 'right', labels: legendLabels(colors, true) },
          tooltip: tooltipBase({ label: function (c) {
            var t = c.dataset.data.reduce(function (a, b) { return a + b; }, 0);
            return ' ' + c.label + ': ' + c.parsed + (t ? ' (' + Math.round(c.parsed * 100 / t) + '%)' : '');
          } }),
        },
      },
    });
  }

  // topRadius rounds only the top-most non-zero segment of a stacked column.
  function topRadius(r) {
    return function (ctx) {
      var ds = ctx.chart.data.datasets, i = ctx.dataIndex;
      for (var k = ctx.datasetIndex + 1; k < ds.length; k++) {
        if (ds[k].data[i]) return 0;
      }
      return { topLeft: r, topRight: r, bottomLeft: 0, bottomRight: 0 };
    };
  }

  // renderTime draws the multi-series UTC-day charts (#1302). A null point is a
  // gap: spanGaps stays false so a no-attempt day breaks the line. Bars are SOLID
  // category colors (no pattern fill); the non-color cues are the tooltip, the
  // legend order (= stack order) and the daily-numbers table. details is the
  // per-day tooltip line (data-chart-detail), or null.
  function renderTime(canvas, type, labels, series, suffix, details) {
    var stacked = type === 'stacked-bar';
    var colors = series.map(function (se) { return stacked ? catColor(se.label) : hitColor; });
    var datasets = series.map(function (se, i) {
      if (stacked) {
        return { label: se.label, data: se.data, backgroundColor: colors[i], borderWidth: 0, borderSkipped: false, borderRadius: topRadius(6), maxBarThickness: 24 };
      }
      // Value-shaded line over a soft fill; the gradient spans the chart area.
      function ramp(stop) {
        return function (ctx) {
          var a = ctx.chart.chartArea;
          if (!a) return STRONG;
          var g = ctx.chart.ctx.createLinearGradient(0, a.bottom, 0, a.top);
          g.addColorStop(0, stop(WEAK, 1)); g.addColorStop(0.5, stop(MID, 1)); g.addColorStop(1, stop(STRONG, 1));
          return g;
        };
      }
      return { label: se.label, data: se.data, spanGaps: false, borderWidth: 3, tension: 0.25, cubicInterpolationMode: 'monotone',
        pointRadius: 3, pointHoverRadius: 7, pointBorderColor: surfaceBg, pointBorderWidth: 1.5,
        pointBackgroundColor: function (ctx) { return ctx.raw === null || ctx.raw === undefined ? STRONG : rampColor(ctx.raw); },
        borderColor: ramp(function (c) { return c; }), fill: 'origin',
        backgroundColor: ramp(function (c) { return rgba(c, 0.18); }) };
    });
    var yScale = { beginAtZero: true, stacked: stacked, grid: { color: gridColor, borderDash: [2, 5] }, border: { display: false }, ticks: { precision: 0, padding: 8 } };
    if (!stacked) { yScale.max = 100; yScale.ticks.callback = function (v) { return v + suffix; }; }
    return new Chart(canvas, {
      type: stacked ? 'bar' : 'line',
      data: { labels: labels, datasets: datasets },
      options: {
        animation: anim, interaction: { mode: 'index', intersect: false },
        scales: { x: { stacked: stacked, grid: { display: false }, border: { display: false }, ticks: { autoSkip: true, maxTicksLimit: 6, maxRotation: 0 } }, y: yScale },
        plugins: {
          legend: { position: 'bottom', labels: legendLabels(colors, false) },
          tooltip: tooltipBase({
            label: function (c) {
              if (c.parsed.y === null) return '';
              return ' ' + c.dataset.label + ': ' + (stacked ? c.parsed.y : c.parsed.y + suffix);
            },
            afterBody: function (items) {
              if (!items.length) return '';
              if (stacked) {
                var tot = 0;
                items[0].chart.data.datasets.forEach(function (d) { tot += d.data[items[0].dataIndex] || 0; });
                return 'Total landings: ' + tot;
              }
              return details ? (details[items[0].dataIndex] || '') : '';
            },
          }),
        },
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
        renderTime(canvas, type, labels, series, canvas.getAttribute('data-chart-suffix') || '',
          canvas.hasAttribute('data-chart-detail') ? parseAttr(canvas, 'data-chart-detail') : null);
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
