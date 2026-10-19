// status_recovered_probe.js —— 账号状态列「陈旧错误快照」守卫（静态探针，无需浏览器）。
//
// # 它守的是什么
//
// `pool.NoteErrorExplain` 刻意**不在成功时清空** last_error（理由充分：
// 成功就清会让偶发失败在界面上永远无痕）。代价是前端拿到的一行数据里，
// 「6 分钟前 503、之后连续 200」与「现在就坏着」**同形** ——
// 唯一的区分依据是两个时刻的先后。
//
// 实测事故（2026-10-19 15:07）：基元律动回了两发 503（LITELLM_UNAVAILABLE），
// 15:08 起全部 200，两个账号熔断计数 0、未冷却，
// 但状态列一直显示「上游故障」—— 用户于是去排一个已经不存在的障。
//
// # 为什么是"抽出来真跑"，而不是 grep
//
// 断言"源码里有 last_success 这个词"证明不了行为：把比较方向写反
//（tOk < tErr）、或用 `>=` 把同一时刻算成已恢复，grep 一样全绿。
// 所以这里把 statusCellHTML + timeAgo 从 webui.html 里抽出来，
// 用固定 fixtures 真跑一遍看输出。
//
// # 变异验证（自带）
//
// 末尾把 `const recovered = ...` 强行改成 `false`，重跑同一组 fixtures，
// 必须**红**。这条证明的是"本探针有判别力"，不是产品行为。
const fs = require('fs');
const path = require('path');

const REPO = process.env.WB2API_REPO || path.join(__dirname, '..', '..');
const html = fs.readFileSync(path.join(REPO, 'internal/server/webui.html'), 'utf8');

let pass = 0;
const fails = [];
function ok(cond, name) {
  if (cond) { pass++; return; }
  fails.push(name);
}

// ---- 抽取两个函数（按标记切片；标记缺失就报清楚，不要静默跳过）----
function slice(startMark, endMark) {
  const i = html.indexOf(startMark);
  const j = html.indexOf(endMark, i + startMark.length);
  if (i < 0) throw new Error('找不到起始标记：' + startMark);
  if (j < 0) throw new Error('找不到结束标记：' + endMark);
  return html.slice(i, j);
}

const cellSrc = slice('function statusCellHTML(a) {', '\n  // timeAgo ');
const agoSrc = slice('function timeAgo(iso) {', '\n  // accountRow ');

// esc 用桩：本探针断言的是**标签文案与 pill 类名**，不是转义细节
//（转义另有 verify_t3_t4_static 之类的守卫）。
function build(src) {
  const factory = new Function('esc', src + '\nreturn { statusCellHTML, timeAgo };');
  return factory(s => String(s == null ? '' : s));
}

const { statusCellHTML } = build(cellSrc + '\n' + agoSrc);

// ---- fixtures：形状取自 /admin/accounts 的**真实**响应 ----
const ERR = {
  code: 'LITELLM_UNAVAILABLE',
  http_status: 503,
  kind: 'server',
  summary: '上游故障',
  detail: '上游返回 5xx……',
  at: '2026-10-19T15:07:35.594000885+08:00',
};
const base = { provider: 'jiyuan', disabled: false, cooling: false, breaker_fails: 0 };

// 1) 已恢复：成功时刻晚于错误时刻 → 正常，标签里不许出现「上游故障」
{
  const row = Object.assign({}, base, {
    last_error: ERR, last_success: '2026-10-19T15:13:24.836787133+08:00',
  });
  const out = statusCellHTML(row);
  ok(out.includes('class="pill ok"'), '已恢复：pill 应为 ok（实得 ' + out + '）');
  ok(out.includes('>正常<'), '已恢复：标签应为「正常」');
  ok(!out.includes('上游故障'), '已恢复：标签不得再写「上游故障」');
  ok(out.includes('已恢复'), '已恢复：title 应说明已恢复（信息不丢，降级到悬浮）');
}

// 2) 未恢复：有错误、且之后没有更晚的成功 → 保留解读，但**标签带时刻**
{
  const row = Object.assign({}, base, {
    last_error: ERR, last_success: '2026-10-19T15:00:00+08:00',
  });
  const out = statusCellHTML(row);
  ok(out.includes('class="pill dim"'), '未恢复：pill 应为 dim（实得 ' + out + '）');
  ok(out.includes('上游故障'), '未恢复：保留上游解读');
  ok(/上游故障 · .+前/.test(out), '未恢复：标签必须带「多久前」，否则被读成当前状态');
}

// 3) 没有 last_success 字段（旧后端 / 从未成功过）→ 退回改造前行为，不炸不误判
{
  const row = Object.assign({}, base, { last_error: ERR });
  const out = statusCellHTML(row);
  ok(out.includes('上游故障'), '无成功记录：仍显示解读');
  ok(!out.includes('已恢复'), '无成功记录：不得谎称已恢复');
}

// 4) 同一时刻（tOk == tErr，时钟相同）不算恢复：证据不足时保守
{
  const row = Object.assign({}, base, { last_error: ERR, last_success: ERR.at });
  const out = statusCellHTML(row);
  ok(!out.includes('已恢复'), '同刻：tOk == tErr 不得判成已恢复');
}

// 5) 回归：冷却 / 熔断 / 禁用 / 无错误 四条分支行为不变
{
  const cooling = statusCellHTML(Object.assign({}, base, {
    cooling: true, cool_remaining_sec: 42, last_error: ERR,
    last_success: '2026-10-19T15:13:24+08:00',
  }));
  ok(cooling.includes('class="pill warn"'), '冷却：仍是 warn（不被"已恢复"盖掉）');
  ok(cooling.includes('42s'), '冷却：仍显示剩余秒数');

  const breaker = statusCellHTML(Object.assign({}, base, { breaker_fails: 3 }));
  ok(breaker.includes('熔断 3'), '熔断：显示计数');

  const off = statusCellHTML(Object.assign({}, base, { disabled: true, last_error: ERR }));
  ok(off.includes('已禁用'), '禁用：优先于一切解读');

  const clean = statusCellHTML(Object.assign({}, base));
  ok(clean.includes('class="pill ok"') && clean.includes('>正常<'), '无错误：正常');
}

// ---- 变异验证：把恢复判据钉死成 false，上面第 1 组必须变红 ----
{
  const mutated = cellSrc.replace(
    /const recovered = [^;]+;/,
    'const recovered = false;'
  );
  if (mutated === cellSrc) {
    fails.push('变异验证：没找到 recovered 判据那一行，守卫未覆盖到目标代码');
  } else {
    const m = build(mutated + '\n' + agoSrc);
    const out = m.statusCellHTML(Object.assign({}, base, {
      last_error: ERR, last_success: '2026-10-19T15:13:24+08:00',
    }));
    ok(!out.includes('class="pill ok"') || out.includes('上游故障'),
      '变异验证：关掉恢复判据后仍显示正常 —— 说明上面的断言没有判别力');
    ok(out.includes('上游故障') && out.includes('·'),
      '变异验证：关掉恢复判据后应回到「上游故障 · 多久前」');
  }
}

if (fails.length) {
  console.error('FAIL  ' + fails.length + ' 条：');
  for (const f of fails) console.error('  - ' + f);
  process.exit(1);
}
console.log('OK    status_recovered_probe：' + pass + ' 条断言全绿（含变异验证）');
