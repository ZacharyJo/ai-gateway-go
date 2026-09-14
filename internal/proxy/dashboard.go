package proxy

// dashboardHTML 是监控仪表盘页面（内嵌，无外部资源），轮询 /api/stats 渲染。
// 功能：KPI 卡片 + 分钟吞吐图 + 状态码分布
// + 最近请求 + 会话聚合。
// 注意：Go 原始字符串用反引号，JS 内不能用模板字符串（反引号），统一用 + 拼接。
const dashboardHTML = `<!doctype html>
<html lang="zh"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ai-gateway 监控</title>
<style>
:root {
  color-scheme: dark;
  --bg: #0c111d;
  --panel: #121a2a;
  --panel2: #17203a;
  --border: #223050;
  --text: #e8edf8;
  --muted: #93a0bd;
  --faint: #5d6a87;
  --accent: #38bdf8;
  --accent2: #a78bfa;
  --ok: #34d399;
  --warn: #fbbf24;
  --err: #f87171;
  --mono: ui-monospace, "SF Mono", SFMono-Regular, Menlo, Consolas, monospace;
}
* { box-sizing: border-box; }
html, body { margin: 0; padding: 0; }
body {
  font-family: system-ui, -apple-system, "PingFang SC", "Microsoft YaHei", sans-serif;
  background:
    radial-gradient(1100px 500px at 85% -10%, rgba(56,189,248,.10), transparent 60%),
    radial-gradient(900px 460px at -10% 0%, rgba(167,139,250,.10), transparent 55%),
    var(--bg);
  color: var(--text);
  min-height: 100vh;
  padding: 1.25rem 1.5rem 2rem;
}
.topbar { display: flex; align-items: flex-start; justify-content: space-between; gap: 1rem; flex-wrap: wrap; margin-bottom: 1.25rem; }
.brand { display: flex; gap: .8rem; align-items: center; }
.logo { width: 40px; height: 40px; border-radius: 11px; display: grid; place-items: center; font-size: 1.15rem; color: #fff; background: linear-gradient(135deg, #38bdf8, #6366f1); box-shadow: 0 4px 14px rgba(56,189,248,.25); }
h1 { font-size: 1.25rem; margin: 0; letter-spacing: .2px; }
.sub { color: var(--muted); font-size: .82rem; margin-top: .15rem; }
.meta { display: flex; gap: .5rem; flex-wrap: wrap; }
.chip { font-size: .78rem; font-family: var(--mono); color: var(--muted); background: var(--panel); border: 1px solid var(--border); border-radius: 999px; padding: .35rem .7rem; }
.chip b { color: var(--text); font-weight: 600; }
.chip.accent { color: var(--accent); }
.cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: .8rem; margin-bottom: 1rem; }
.card { position: relative; overflow: hidden; background: linear-gradient(180deg, var(--panel2), var(--panel)); border: 1px solid var(--border); border-radius: 14px; padding: .85rem 1rem .8rem; }
.card::before { content: ""; position: absolute; left: 0; right: 0; top: 0; height: 2px; background: var(--cardc, var(--accent)); opacity: .85; }
.card .top { display: flex; align-items: center; justify-content: space-between; }
.card .ico { width: 26px; height: 26px; border-radius: 8px; display: grid; place-items: center; color: var(--cardc, var(--accent)); background: rgba(56,189,248,.10); }
.card .ico svg { width: 15px; height: 15px; }
.card .n { font-size: 1.45rem; font-weight: 700; font-family: var(--mono); margin-top: .5rem; letter-spacing: -.5px; }
.card .k { font-size: .72rem; color: var(--muted); }
.card[data-c="ok"] { --cardc: var(--ok); }
.card[data-c="warn"] { --cardc: var(--warn); }
.card[data-c="bad"] { --cardc: var(--err); }
.grid { display: grid; grid-template-columns: 2fr 1fr; gap: .9rem; margin-bottom: .9rem; }
@media (max-width: 900px) { .grid { grid-template-columns: 1fr; } }
.panel { background: var(--panel); border: 1px solid var(--border); border-radius: 14px; padding: 1rem 1.1rem; margin-bottom: .9rem; }
.grid .panel { margin-bottom: 0; }
.panel h2 { font-size: .86rem; margin: 0 0 .7rem; color: #c7d1e6; font-weight: 600; display: flex; align-items: baseline; gap: .5rem; }
.hint { font-size: .72rem; color: var(--faint); font-weight: 400; }
.chart { width: 100%; height: auto; display: block; }
.chart .cgrid { stroke: var(--border); stroke-width: 1; stroke-dasharray: 3 4; }
.chart .caxis { fill: var(--faint); font-size: 10px; font-family: var(--mono); }
.chart .area { fill: url(#af); }
.chart .line { fill: none; stroke: var(--ok); stroke-width: 2; stroke-linecap: round; }
.chart .errpt { fill: var(--err); stroke: var(--bg); stroke-width: 1.5; }
.legend { display: flex; gap: 1rem; margin-top: .5rem; font-size: .72rem; color: var(--muted); }
.lg { display: inline-flex; align-items: center; gap: .35rem; }
.lg::before { content: ""; width: 9px; height: 9px; border-radius: 3px; background: var(--c); }
.lg.req { --c: var(--accent); }
.lg.resp { --c: var(--ok); }
.lg.err { --c: var(--err); }
.stack { display: flex; height: 14px; border-radius: 7px; overflow: hidden; margin-bottom: .8rem; border: 1px solid var(--border); }
.stack .seg { height: 100%; }
.stack .seg.ok { background: var(--ok); }
.legend-list { display: grid; gap: .45rem; font-size: .78rem; }
.ll { display: flex; align-items: center; gap: .5rem; }
.ll .dot { width: 9px; height: 9px; border-radius: 3px; flex: none; }
.ll .lab { color: var(--muted); flex: 1; }
.ll .cnt { font-family: var(--mono); color: var(--text); }
.ll .pct { color: var(--faint); font-family: var(--mono); width: 3.4em; text-align: right; }
.table-wrap { overflow-x: auto; }
table { width: 100%; border-collapse: collapse; font-size: .8rem; }
th, td { text-align: left; padding: .45rem .55rem; border-bottom: 1px solid var(--border); white-space: nowrap; }
th { color: var(--faint); font-weight: 500; font-size: .72rem; letter-spacing: .2px; }
td.num, th.num { text-align: right; font-variant-numeric: tabular-nums; font-family: var(--mono); }
tbody tr { transition: background .15s; }
tbody tr:hover { background: rgba(56,189,248,.05); }
tr.err td { color: var(--err); }
tr.warn td { color: var(--warn); }
td.path { max-width: 320px; overflow: hidden; text-overflow: ellipsis; }
td.src { font-family: var(--mono); color: var(--accent); }
.sbar { width: 64px; height: 5px; background: var(--border); border-radius: 3px; overflow: hidden; margin-top: .25rem; }
.sbar div { height: 100%; background: linear-gradient(90deg, var(--accent), var(--accent2)); border-radius: 3px; }
.pill { display: inline-block; border-radius: 999px; padding: .05rem .5rem; font-size: .68rem; line-height: 1.5; }
.pill.lvl { background: var(--border); color: var(--muted); }
.pill.lvl.error { background: rgba(248,113,113,.15); color: var(--err); }
.pill.lvl.warn { background: rgba(251,191,36,.15); color: var(--warn); }
.pill.lvl.info { background: rgba(56,189,248,.15); color: var(--accent); }
.pill.st { background: rgba(52,211,153,.14); color: var(--ok); }
.pill.st.warn { background: rgba(251,191,36,.15); color: var(--warn); }
.pill.st.bad { background: rgba(248,113,113,.15); color: var(--err); }
.pill.method { background: rgba(167,139,250,.14); color: var(--accent2); font-family: var(--mono); }
.empty { color: var(--faint); font-size: .8rem; padding: .8rem .5rem; }
footer { margin-top: 1rem; color: var(--faint); font-size: .72rem; text-align: center; }
</style></head>
<body>
<header class="topbar">
  <div class="brand">
    <div class="logo">&#9671;</div>
    <div>
      <h1>ai-gateway 监控</h1>
      <div class="sub" id="sub">加载中…</div>
    </div>
  </div>
  <div class="meta">
    <span class="chip" id="chipUp">上游 —</span>
    <span class="chip" id="chipRun">运行 —</span>
    <span class="chip accent" id="chipClock">--:--:--</span>
  </div>
</header>

<div class="cards" id="cards"></div>

<div class="grid">
  <section class="panel">
    <h2>每分钟吞吐 <span class="hint">最近 60 分钟</span></h2>
    <svg id="chart" viewBox="0 0 820 220" class="chart"></svg>
    <div class="legend">
      <span class="lg req">请求</span><span class="lg resp">完成</span><span class="lg err">错误</span>
    </div>
  </section>
  <section class="panel">
    <h2>状态码分布</h2>
    <div id="statusBars"></div>
  </section>
</div>

<section class="panel">
  <h2>最近请求 <span class="hint" id="evCount"></span></h2>
  <div class="table-wrap">
  <table id="events"><thead><tr>
  <th>时间</th><th>级别</th><th>事件</th><th class="num">请求号</th><th>方法</th><th>路径</th>
  <th class="num">状态码</th><th class="num">重试</th><th class="num">耗时ms</th></tr></thead>
  <tbody id="eventRows"></tbody></table>
  </div>
</section>

<section class="panel">
  <h2>会话聚合 <span class="hint" id="srcCount"></span></h2>
  <div class="table-wrap">
  <table id="sources"><thead><tr>
  <th>会话</th><th class="num">请求</th><th class="num">响应</th><th class="num">429重试</th>
  <th class="num">错误</th><th class="num">非200</th><th class="num">平均ms</th><th class="num">最大ms</th></tr></thead>
  <tbody id="sourceRows"></tbody></table>
  </div>
</section>

<footer>每 5 秒自动刷新 · 数据仅存于进程内存，重启清零</footer>
<script>
var fmt = function (n) { return n == null ? "" : n.toLocaleString(); };
var esc = function (s) { return String(s == null ? "" : s).replace(/[&<>"]/g, function (c) {
  return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]; }); };
var pad2 = function (n) { return (n < 10 ? "0" : "") + n; };
var hmss = function (ts) { var d = new Date(ts); return pad2(d.getHours()) + ":" + pad2(d.getMinutes()) + ":" + pad2(d.getSeconds()); };
var fmtUp = function (s) {
  var d = Math.floor(s / 86400), h = Math.floor(s % 86400 / 3600), m = Math.floor(s % 3600 / 60), sec = s % 60;
  var out = "";
  if (d > 0) out += d + "天 ";
  return out + pad2(h) + ":" + pad2(m) + ":" + pad2(sec);
};
var statusColor = function (code) {
  if (code < 300) return "#34d399";
  if (code === 429) return "#fb923c";
  if (code < 500) return "#fbbf24";
  return "#f87171";
};
var stCls = function (code) {
  if (code < 300) return "";
  if (code === 429 || code < 500) return "warn";
  return "bad";
};

var icons = {
  req: '<path d="M3 8h10M9 4l4 4-4 4"/>',
  resp: '<path d="M3.5 8.5l3 3 6-7"/>',
  gauge: '<path d="M4 12a5 5 0 1 1 7 4.6"/><path d="M8 9l3-3"/><circle cx="8" cy="9" r="1"/>',
  retry: '<path d="M13.5 7.5a5 5 0 1 0-1.5-3.6"/><path d="M13 2.5V6h-3.5"/>',
  wait: '<path d="M4.5 3h7M4.5 13h7M6 3l2 3 2-3M6 13l2-3 2 3"/>',
  err: '<path d="M8 3l6 11H2z"/><path d="M8 8v3M8 12.5h.01"/>',
  warn: '<circle cx="8" cy="8" r="6"/><path d="M8 5v4M8 11.5h.01"/>'
};
var iconSVG = function (name) {
  return '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">' + (icons[name] || "") + "</svg>";
};

function renderCards(s) {
  var ok = s.responses - s.non200.total;
  if (ok < 0) ok = 0;
  var rate = s.responses > 0 ? (ok / s.responses * 100) : 0;
  var rateTxt = s.responses > 0 ? rate.toFixed(0) + "%" : "-";
  var rateCls = s.responses > 0 ? (rate >= 95 ? "ok" : (rate >= 85 ? "warn" : "bad")) : "";
  var cards = [
    { k: "请求", v: fmt(s.requests), c: "", icon: "req" },
    { k: "响应", v: fmt(s.responses), c: "ok", icon: "resp" },
    { k: "成功率", v: rateTxt, c: rateCls, icon: "gauge" },
    { k: "上游重试", v: fmt(s.retriesUpstream), c: "warn", icon: "retry" },
    { k: "429冷却", v: fmt(s.retries429), c: "warn", icon: "wait" },
    { k: "错误", v: fmt(s.errors), c: s.errors > 0 ? "bad" : "", icon: "err" },
    { k: "非200", v: fmt(s.non200.total), c: s.non200.total > 0 ? "bad" : "", icon: "warn" }
  ];
  var h = "";
  cards.forEach(function (c) {
    h += '<div class="card" data-c="' + c.c + '">'
      + '<div class="top"><span class="k">' + c.k + "</span><span class=\"ico\">" + iconSVG(c.icon) + "</span></div>"
      + '<div class="n">' + c.v + "</div></div>";
  });
  document.getElementById("cards").innerHTML = h;
}

function renderChart(src) {
  var svg = document.getElementById("chart");
  if (!src || src.length === 0) {
    svg.innerHTML = '<text x="410" y="115" class="caxis" text-anchor="middle">暂无数据</text>';
    return;
  }
  // 只有一个分钟桶时（刚启动/流量集中在同一分钟）补一个前置零点，
  // 否则折线退化成单点、X() 会除零，图表会一直空白。
  var tl = src;
  if (tl.length === 1) {
    tl = [{ t: src[0].t - 60, r: 0, s: 0, e: 0 }, src[0]];
  }
  var W = 820, H = 220, pad = 28;
  var max = 1;
  for (var i = 0; i < tl.length; i++) {
    if (tl[i].r > max) max = tl[i].r;
    if (tl[i].s > max) max = tl[i].s;
    if (tl[i].e > max) max = tl[i].e;
  }
  var iw = W - pad - 8, ih = H - pad - 18;
  var X = function (i) { return pad + iw * i / (tl.length - 1); };
  var Y = function (v) { return pad + ih - ih * v / max; };
  var h = "";
  for (var g = 0; g <= 4; g++) {
    var gy = pad + ih * g / 4;
    var val = Math.round(max * (4 - g) / 4);
    h += '<line class="cgrid" x1="' + pad + '" y1="' + gy.toFixed(1) + '" x2="' + (W - 8) + '" y2="' + gy.toFixed(1) + '"/>';
    h += '<text class="caxis" x="' + (pad - 6) + '" y="' + (gy + 3).toFixed(1) + '" text-anchor="end">' + val + "</text>";
  }
  // 请求面积
  var ar = "";
  for (var i2 = 0; i2 < tl.length; i2++) {
    var px = X(i2), py = Y(tl[i2].r);
    ar += (i2 === 0 ? "M" : "L") + px.toFixed(1) + " " + py.toFixed(1);
  }
  ar += " L" + X(tl.length - 1).toFixed(1) + " " + Y(0).toFixed(1)
      + " L" + X(0).toFixed(1) + " " + Y(0).toFixed(1) + " Z";
  h += '<defs><linearGradient id="af" x1="0" y1="0" x2="0" y2="1">'
    + '<stop offset="0" stop-color="#38bdf8" stop-opacity=".45"/>'
    + '<stop offset="1" stop-color="#38bdf8" stop-opacity="0"/></linearGradient></defs>';
  h += '<path class="area" d="' + ar + '"/>';
  // 完成线
  var ln = "";
  for (var i3 = 0; i3 < tl.length; i3++) {
    var lx = X(i3), ly = Y(tl[i3].s);
    ln += (i3 === 0 ? "M" : "L") + lx.toFixed(1) + " " + ly.toFixed(1);
  }
  h += '<path class="line" d="' + ln + '"/>';
  // 错误点
  for (var i4 = 0; i4 < tl.length; i4++) {
    if (tl[i4].e > 0) {
      h += '<circle class="errpt" cx="' + X(i4).toFixed(1) + '" cy="' + Y(tl[i4].e).toFixed(1) + '" r="3">'
        + "<title>" + tl[i4].e + " 个错误</title></circle>";
    }
  }
  // X 轴时间标签（首/中/尾）：去重避免点数少时重叠，两端内收避免被裁掉
  var marks = [0, Math.floor((tl.length - 1) / 2), tl.length - 1];
  var seen = {};
  for (var mi = 0; mi < marks.length; mi++) {
    var mi2 = marks[mi];
    if (seen[mi2]) { continue; }
    seen[mi2] = 1;
    var anchor = mi2 === 0 ? "start" : (mi2 === tl.length - 1 ? "end" : "middle");
    var d = new Date(tl[mi2].t * 1000);
    h += '<text class="caxis" x="' + X(mi2).toFixed(1) + '" y="' + (H - 4) + '" text-anchor="' + anchor + '">'
      + pad2(d.getHours()) + ":" + pad2(d.getMinutes()) + "</text>";
  }
  svg.innerHTML = h;
}

function renderStatus(s) {
  var el = document.getElementById("statusBars");
  var ok = s.responses - s.non200.total;
  if (ok < 0) ok = 0;
  var total = ok + s.non200.total;
  if (total <= 0) { el.innerHTML = '<div class="empty">暂无数据</div>'; return; }
  var bs = s.non200.byStatus || {};
  var codes = Object.keys(bs).map(function (k) { return parseInt(k, 10); }).filter(function (n) { return !isNaN(n); });
  codes.sort(function (a, b) { return a - b; });
  var segs = "";
  if (ok > 0) segs += '<div class="seg ok" style="width:' + (ok / total * 100).toFixed(1) + '%"></div>';
  codes.forEach(function (code) {
    var c = bs[String(code)];
    if (!c) return;
    segs += '<div class="seg" style="width:' + (c / total * 100).toFixed(1) + '%;background:' + statusColor(code) + '"></div>';
  });
  var list = '<div class="ll"><span class="dot" style="background:#34d399"></span>'
    + '<span class="lab">成功</span><span class="cnt">' + fmt(ok) + "</span><span class=\"pct\">" + (ok / total * 100).toFixed(0) + "%</span></div>";
  codes.forEach(function (code) {
    var c = bs[String(code)];
    var pct = (c / total * 100).toFixed(0);
    list += '<div class="ll"><span class="dot" style="background:' + statusColor(code) + '"></span>'
      + '<span class="lab">' + code + "</span><span class=\"cnt\">" + fmt(c) + "</span><span class=\"pct\">" + pct + "%</span></div>";
  });
  el.innerHTML = '<div class="stack">' + segs + '</div><div class="legend-list">' + list + "</div>";
}

function renderEvents(evs) {
  var rows = "";
  (evs || []).slice(0, 40).forEach(function (e) {
    var cls = e.level === "error" ? "err" : (e.level === "warn" ? "warn" : "");
    var lvl = e.level === "error" ? "error" : (e.level === "warn" ? "warn" : "info");
    // 固定取值集，不来自上游数据，无需 esc
    var lvlTxt = e.level === "error" ? "错误" : (e.level === "warn" ? "警告" : "信息");
    var trys = e.try ? e.try + "/" + (e.maxTry == null ? "" : e.maxTry) : "";
    var st = "";
    if (e.status != null) st = '<span class="pill st ' + stCls(e.status) + '">' + e.status + "</span>";
    var md = "";
    if (e.method) md = '<span class="pill method">' + esc(e.method) + "</span>";
    rows += '<tr class="' + cls + '"><td>' + (e.ts ? hmss(e.ts) : "") + "</td>"
      + '<td><span class="pill lvl ' + lvl + '">' + lvlTxt + "</span></td>"
      + "<td>" + esc(e.name) + "</td>"
      + '<td class="num">' + (e.req == null ? "" : e.req) + "</td>"
      + "<td>" + md + "</td>"
      + '<td class="path" title="' + esc(e.path) + '">' + esc(e.path) + "</td>"
      + '<td class="num">' + st + "</td>"
      + '<td class="num">' + trys + "</td>"
      + '<td class="num">' + (e.durMs == null ? "" : e.durMs) + "</td></tr>";
  });
  document.getElementById("eventRows").innerHTML = rows || '<tr><td class="empty" colspan="9">暂无请求</td></tr>';
  document.getElementById("evCount").textContent = "(" + (evs ? evs.length : 0) + " 条)";
}

function renderSources(list) {
  var rows = "";
  var maxReq = 1;
  (list || []).forEach(function (v) { if (v.requests > maxReq) maxReq = v.requests; });
  (list || []).forEach(function (v) {
    var bar = "";
    if (v.requests > 0) bar = '<div class="sbar"><div style="width:' + (v.requests / maxReq * 100).toFixed(0) + '%"></div></div>';
    rows += "<tr>"
      + '<td class="src">' + esc(v.source) + bar + "</td>"
      + '<td class="num">' + fmt(v.requests) + "</td>"
      + '<td class="num">' + fmt(v.responses) + "</td>"
      + '<td class="num">' + fmt(v.retries429) + "</td>"
      + '<td class="num ' + (v.errors > 0 ? "err" : "") + '">' + fmt(v.errors) + "</td>"
      + '<td class="num ' + (v.non200 > 0 ? "warn" : "") + '">' + fmt(v.non200) + "</td>"
      + '<td class="num">' + fmt(v.avgDurMs) + "</td>"
      + '<td class="num">' + fmt(v.maxDurMs) + "</td></tr>";
  });
  document.getElementById("sourceRows").innerHTML = rows || '<tr><td class="empty" colspan="8">暂无会话</td></tr>';
  document.getElementById("srcCount").textContent = "(" + (list ? list.length : 0) + " 个)";
}

function load() {
  fetch("/api/stats", {cache: "no-store"}).then(function (r) { return r.json(); }).then(function (s) {
    document.getElementById("sub").textContent =
      "上游 " + s.upstreamBase + " · 运行 " + fmtUp(s.uptimeSec) + " · " + s.now;
    document.getElementById("chipUp").innerHTML = "上游 <b>" + esc(s.upstreamBase) + "</b>";
    document.getElementById("chipRun").innerHTML = "运行 <b>" + fmtUp(s.uptimeSec) + "</b>";
    renderCards(s);
    renderChart(s.timeline);
    renderStatus(s);
    renderEvents(s.recentEvents);
    renderSources(s.sources);
  }).catch(function (e) {
    document.getElementById("sub").textContent = "加载失败: " + e;
  });
}
load();
setInterval(load, 5000);
// 本地秒级时钟：数据 5s 才刷新，时钟不能跟着跳
setInterval(function () {
  var d = new Date();
  document.getElementById("chipClock").textContent = pad2(d.getHours()) + ":" + pad2(d.getMinutes()) + ":" + pad2(d.getSeconds());
}, 1000);
</script>
</body></html>
`
