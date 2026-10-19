// 生成自包含测试：调用统计的**卡片口径**与窗口文案。
//
// # 历史与改名（stats_disk → stats_cards）
//
// 本套件原来叫 stats_disk_gen，只测「落盘占用」那张卡。后来两次产品改动把它
// 的事实基础抽掉了：
//
//	① 统计卡精简 —— 「落盘占用」「缓冲占用」「缓存命中率」等排障细节被移除，
//	   卡片只剩 总请求/成功/失败/成功率/三个 token 卡/两个耗时。
//	② token 口径修正 —— 「消耗 token」从 `tokens + think_tokens`（reasoning 被
//	   加了两遍，高报 58%）改成后端的 total_tokens = 输入 + 输出。
//
// 旧套件因此**一直在测已删除的功能**：它断言 cardValue('落盘占用')，而那两张
// 卡早就不存在了，取到 null，断言全红。它同时还缺 `window` 桩与
// `statsCardsEntries` 抽取，所以真实表现是「崩在 setup 之前」——
// 崩得比红更难读，于是长期没人认出这是陈旧套件而不是产品坏了。
//
// 现在改成测**当前存在的卡片**，并顺手把 token 三卡的口径钉在流程层
// （Go 侧 webui_stats_tokens_test.go 是源码级守卫，这里补行为级）。
//
// # 覆盖
//
//  1. 窗口文案只报条数，不回显「从某时刻起」
//  2. 卡片清单与顺序符合当前产品（删掉的卡不许回来，token 三卡必须在）
//  3. 「消耗 token」= 后端 total_tokens，且**不得**出现 reasoning 双计
//  4. 病态 payload（对象/数组/垃圾串/NaN/Infinity）不许把 [object Object] 显示给用户
const fs = require('fs');

// (process.env.WB2API_REPO || __dirname + '/../..')：仓库根。本文件位于 tests/frontend/，距根两级。
// 用 __dirname 而不是相对 cwd —— 从任何目录调用都能找对位置。
// WB2API_REPO 可覆盖（变异扫描用它指向一次性副本）。

const path = (process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html';
const html = fs.readFileSync(path, 'utf8');

function grabBlock(startMarker) {
  const start = html.indexOf(startMarker);
  if (start < 0) throw new Error('找不到: ' + startMarker);
  let i = html.indexOf('{', start), depth = 0;
  for (; i < html.length; i++) {
    if (html[i] === '{') depth++;
    else if (html[i] === '}') { depth--; if (depth === 0) return html.slice(start, i + 1); }
  }
  throw new Error('括号不闭合: ' + startMarker);
}

// 从真实源码抽函数时，要连**它调用的其它函数**一起抽，不能只抽入口。
//
// 这条教训踩过两次：
//   - 新增 num() 后报 `ReferenceError: num is not defined`
//   - 卡片内容被抽成 statsCardsEntries 后报 `ReferenceError: statsCardsEntries is not defined`
//
// 所以下面的清单是 renderStats + statsCardsEntries 的**完整依赖闭包**。
// 将来再抽出新函数，这份清单必须同步 —— 漏一个的表现是崩，不是红。
const pieces = [
  grabBlock('function num('),
  grabBlock('function numNonNeg('),
  grabBlock('function fmtCount('),
  grabBlock('function fmtTokens('),
  grabBlock('function renderStatsModels('),
  grabBlock('function statsCardsEntries('),
  grabBlock('function renderStats('),
];

const header = `
const fs = require('fs');
const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');
// 模块级常量：renderStats 用到 NODATA（"无数据"占位）。从源码取，避免两处漂移。
const NODATA = /const NODATA = '([^']*)'/.exec(html)[1];
const els = {};
// window 桩：renderStats 开头有一句 \`window._statsCache = s\`（供 renderCards
// 读累计请求数）。Node 里没有 window，缺这个桩会在**第一行**就抛
// \`ReferenceError: window is not defined\` —— 表现是整个套件直接崩在 setup 之前，
// 而不是某条断言变红，所以它长期以"崩"的形式出现、没人认出是缺桩。
//
// 用普通对象而不是 globalThis：渲染逻辑只往里写一个属性再读，不需要浏览器语义。
const window = {};
// style 必须存在：renderStatsModels 会设 wrap.style.display
function mkEl(id) { return { id, hidden: true, className: '', textContent: '', innerHTML: '', disabled: false, style: {} }; }
for (const id of ['statsWindow','statsCards','statsModels','statsModelsWrap','statsCatalogHint']) els[id] = mkEl(id);
const $ = id => els[id] || (els[id] = mkEl(id));
// esc 在源码里是 const 箭头函数（不是 function 声明），grabBlock 抽不到，
// 这里照抄实现（它只影响转义，与本次要验证的渲染逻辑无关）。
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
function resetEls() { for (const k of Object.keys(els)) { const e = els[k]; e.textContent=''; e.innerHTML=''; } }
// 从渲染出的卡片 HTML 里取某个 key 对应的值（卡片结构：k 一行、v 一行）
function cardValue(key) {
  const h = $('statsCards').innerHTML;
  const i = h.indexOf('>' + key + '<');
  if (i < 0) return null;
  const m = /<div class="v[^"]*">([^<]*)<\\/div>/.exec(h.slice(i));
  return m ? m[1] : null;
}
// 卡片清单（按渲染顺序），供"删掉的卡不许回来 / 该有的必须在"两类断言共用。
function cardKeys() {
  const h = $('statsCards').innerHTML;
  return [...h.matchAll(/data-stat-key="([^"]*)"/g)].map(m => m[1]);
}
`;

const tail = String.raw`
let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

// 夹具形状取自 /admin/stats 的真实响应（含本轮新增的三个 token 键）。
const base = {
  total: 743, ok: 700, fail: 43, avg_ttfb_ms: 120, avg_total_ms: 900,
  tokens: 12345, prompt_tokens: 900000, total_tokens: 912345,
  source: 'file', window_from: '2026-09-11T06:59:39+08:00',
  capacity: 500, held: 120, by_model: { 'deepseek-v4-flash': 12 },
};

// ---------- 1. 窗口文案只报条数 ----------
console.log('\n[1] 窗口文案只报条数');
resetEls(); renderStats({ ...base });
const w = $('statsWindow').textContent;
ok(w === '（全部落盘历史，共 743 条）', '文案为「（全部落盘历史，共 743 条）」（实际：' + w + '）');
ok(!w.includes('06:59:39') && !w.includes('2026'), '不再回显起始日期/时刻');
ok(!w.includes('起'), '不再出现「…起」这种以某时刻为边界的措辞');

// window_from 有没有都不影响该分支的文案
resetEls(); renderStats({ ...base, window_from: '' });
ok($('statsWindow').textContent === '（全部落盘历史，共 743 条）', 'window_from 为空时文案不变');

// ---------- 2. 卡片清单符合当前产品 ----------
console.log('\n[2] 卡片清单');
resetEls(); renderStats({ ...base });
const keys = cardKeys();
// 已删除的卡不许悄悄回来：它们测的是"落盘上限/缓冲占用/缓存命中"这类排障细节，
// 当初被移出首屏是为了让"网关有没有用起来"一眼可读。
for (const gone of ['落盘占用', '缓冲占用', '缓存命中率', '推理 token']) {
  ok(!keys.includes(gone), '已下线的卡片「' + gone + '」不该回到首屏（实际清单：' + keys.join(' / ') + '）');
}
// 该有的必须在 —— 少一张卡是静默的功能缺失，页面上只是"看起来简单了些"。
for (const want of ['总请求', '成功', '失败', '成功率', '消耗 token', '输入 token', '输出 token', '平均 TTFB', '平均总耗时']) {
  ok(keys.includes(want), '卡片「' + want + '」必须存在（实际清单：' + keys.join(' / ') + '）');
}

// ---------- 3. token 三卡的口径 ----------
console.log('\n[3] token 三卡口径');
resetEls(); renderStats({ ...base });
ok(cardValue('输入 token') === '900k', '输入 token 读后端 prompt_tokens（实际：' + cardValue('输入 token') + '）');
ok(cardValue('输出 token') === '12k', '输出 token 读后端 tokens（实际：' + cardValue('输出 token') + '）');
ok(cardValue('消耗 token') === '912k', '消耗 token 读后端 total_tokens（实际：' + cardValue('消耗 token') + '）');

// 口径的核心判据：**不得**出现 reasoning 双计。
//
// 反例形态是 tokens + think_tokens。给一个 think_tokens 远大于 tokens 的
// 病态夹具：若页面把两者相加，"消耗"会变成一个离谱的大数。这里用
// total_tokens 与"输入+输出"一致来反证它没被加第二遍。
resetEls(); renderStats({ ...base, tokens: 1000, prompt_tokens: 5000, total_tokens: 6000, think_tokens: 800 });
ok(cardValue('消耗 token') === '6.0k',
  '消耗 token 严格等于 total_tokens（1000 输出 + 5000 输入 = 6000），reasoning 未被加第二遍（实际：' + cardValue('消耗 token') + '）');

// 旧后端（没有 total_tokens）时退回"输入+输出"，而不是退回到 tokens+reasoning。
resetEls(); renderStats({ ...base, tokens: 1000, prompt_tokens: 5000, total_tokens: undefined, think_tokens: 800 });
ok(cardValue('消耗 token') === '6.0k',
  '缺 total_tokens 时退回 输入+输出（实际：' + cardValue('消耗 token') + '）');

// 三个数全缺 → 显示占位符，而不是 0（0 会被读成"这次真的没用量"）。
resetEls(); renderStats({ ...base, tokens: undefined, prompt_tokens: undefined, total_tokens: undefined });
ok(cardValue('消耗 token') === NODATA, '三个 token 值都缺时显示占位符 ' + NODATA + '（实际：' + cardValue('消耗 token') + '）');
ok(cardValue('输入 token') === NODATA, '输入 token 缺失时显示占位符（实际：' + cardValue('输入 token') + '）');

// ---------- 4. 边界：内存来源 / 空日志 / 病态 payload ----------
console.log('\n[4] 边界情况');
resetEls(); renderStats({ ...base, source: 'memory' });
ok($('statsWindow').textContent === '（仅内存缓冲，进程启动至今，上限 500 条）', '内存来源窗口文案未受影响');

resetEls(); renderStats({ ...base, total: 0, ok: 0, fail: 0, window_from: '' });
ok($('statsWindow').textContent === '（落盘日志为空）', '总数为 0 时提示「落盘日志为空」');

// 窗口文案里的数字也必须防御式格式化。
//
// 回归背景：该行原本是裸插值（模板字符串里直接写 s.total），后端若给出非数值
// （对象/数组/垃圾串），非空值会被串成 [object Object] 直接显示给用户。
// 实测（穷举 15 组病态 payload）抓出：卡片区全部干净，唯独窗口文案漏了。
// 现与卡片区共用 fmtCount。
console.log('\n[4.5] 病态 payload 不许泄漏到界面');
for (const [name, val] of [
  ['对象', {}], ['数组', [1, 2]], ['垃圾字符串', 'abc'],
  ['NaN', NaN], ['undefined', undefined], ['Infinity', Infinity],
]) {
  resetEls(); renderStats({ ...base, total: val, ok: val });
  const t = $('statsWindow').textContent;
  ok(!/\[object|NaN|undefined|Infinity/.test(t),
    'total=' + name + ' 时窗口文案干净（实际：' + t + '）');
  const cards = $('statsCards').innerHTML;
  ok(!/\[object|NaN|undefined|Infinity/.test(cards),
    'total=' + name + ' 时卡片区干净');
}
// 正常值仍要如实显示
resetEls(); renderStats({ ...base, total: 743 });
ok($('statsWindow').textContent === '（全部落盘历史，共 743 条）', '正常数值仍如实显示');
ok(cardValue('总请求') === '743', '总请求仍正常');
ok(cardValue('成功率') === '94%', '成功率仍正常（700/743≈94%）');

// ---------- 5. 静态：口径字段不许被前端自己拼 ----------
console.log('\n[5] 静态检查');
const src = (html.slice(html.indexOf('function statsCardsEntries('), html.indexOf('function renderStatsModels(')));
ok(!/think_tokens/.test(src), 'statsCardsEntries 内不出现 think_tokens（reasoning 双计的指纹）');
ok(/total_tokens/.test(src) && /prompt_tokens/.test(src), 'statsCardsEntries 读取后端 total_tokens / prompt_tokens');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
`;

const out = header + '\n' + pieces.join('\n\n') + '\n' + tail;
fs.writeFileSync('stats_cards_gen.js', out);
console.log('生成: stats_cards_gen.js (' + out.length + ' 字节)');
pieces.forEach(p => console.log('  ' + p.split('\n')[0].trim()));
