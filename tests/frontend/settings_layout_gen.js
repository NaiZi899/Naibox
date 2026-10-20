
const fs = require('fs');
const BASELINE = (process.env.WB2API_REPO || __dirname + '/../..') + '/tests/frontend/settings-baseline-ids.txt';
const WEBUI = (process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html';
const html = fs.readFileSync(WEBUI, 'utf8');

// 只截设置面板那一段：从 <h2>设置 到这个 section 的结束标签。
//
// # 为什么终点不能用 <div id="modal">（T6 踩到的坑）
//
// 原先切到模态框为止。T6 在设置 section 之后、模态框之前插入了
// 「动态上游面板宿主」块（含一句带 ⚠ 的 HTML 注释），于是这段切片
// 把**不属于设置面板**的内容也包了进来，emoji 断言因此假红。
//
// 正确做法是按 DOM 结构切：设置面板的 <h2> 属于某个 <section>，
// 切到该 section 的闭合标签为止。
const SET_START = html.indexOf('<h2>设置');
const SET_END = html.indexOf('</section>', SET_START);
if (SET_END < 0) throw new Error('找不到设置面板的 </section>');
const sec = html.slice(SET_START, SET_END);

// 按 DOM 顺序切出每个 fieldset 的内部
function fieldsets(src) {
  const out = [];
  const re = /<fieldset([^>]*)>([\s\S]*?)<\/fieldset>/g;
  let m;
  while ((m = re.exec(src))) {
    const legend = /<legend>([\s\S]*?)<\/legend>/.exec(m[2]);
    out.push({ attrs: m[1] || '', body: m[2], legend: legend ? legend[1].trim() : null });
  }
  return out;
}

// legend 的**纯文案**：砍掉 <span class="badge">…</span> 这类标记。
// 文案是契约（"组名叫什么、按什么顺序"），badge 的配色不是 —— 分开断言，
// 免得改一个徽章配色就让"文案未变"这条一起红。
function legendText(s) {
  return (s.legend || '').split('<')[0].trim();
}

// 取一个 fieldset 内 .field 的类型序列（按 DOM 顺序）
// .field 与 .field.chk 都算；用「本 .field 的起始位置」排序后逐个判类
function fieldKinds(body) {
  const kinds = [];
  const re = /<div class="field( chk[^"]*)?">/g;
  let m;
  while ((m = re.exec(body))) kinds.push(m[1] ? 'CHK' : 'IN');
  return kinds;
}
function flips(kinds) {
  let n = 0;
  for (let i = 1; i < kinds.length; i++) if (kinds[i] !== kinds[i - 1]) n++;
  return n;
}

// 「分离」的准确判据：类型序列必须是「同类连续」的 A…AB…B 形式，
// 即**最多一次翻转**，且两类各自成段。
//
// 为什么不是「翻转 = 0」：把复选框与输入框分成两个容器后，两者之间必然有
// 一次类型切换（CHK…CHK → IN…IN）。那一次切换正是「分离」本身，不是交叉。
// 真正的交叉是 A…AB…A 这种来回跳（翻转 ≥ 2，或同类型被另一类型打断）。
// 所以判据取 flips <= 1 且等于「非空类型种类数 - 1」。
function isSeparated(kinds) {
  if (kinds.length === 0) return true;
  const kindsPresent = new Set(kinds).size;
  return flips(kinds) === kindsPresent - 1;
}

// 结构扫描：用 div 栈判定每个 .field 落在哪类容器里。
//
// # 为什么不能继续用「把 .fieldgroup 抠掉看剩几个」的区间法
//
// 老实现用正则 /<div class="fieldgroup[^"]*">[\s\S]*?<\/div>\s*<\/div>/
// 去抠容器区间。嵌套一深（容器里再套容器、字段自带收尾 </div>），
// 这个非贪婪匹配就切不准：它会漏数已分组的字段，于是
// 「未分组 = 总数 - 已分组」凭空变大 —— 那是**解析器的错**，
// 却长得像实现的错，最难查的一类假红。
//
// 现在改成显式维护 div 栈：进 <div ...> 压栈、遇 </div> 出栈，
// 遇到 .field 时栈里有没有 fieldgroup / formgrid 一目了然。
// 判据与缩进、换行、注释位置全部无关。
function scanFields(body) {
  const out = [];
  const re = /<div\b([^>]*)>|<\/div>/g;
  const stack = [];
  let m;
  while ((m = re.exec(body))) {
    if (m[0] === '</div>') { if (stack.length) stack.pop(); continue; }
    const cls = (/class="([^"]*)"/.exec(m[1] || '') || [null, ''])[1];
    const kinds = cls.split(/\s+/).filter(Boolean);
    if (kinds.includes('field')) {
      out.push({
        kind: kinds.includes('chk') ? 'CHK' : 'IN',
        inFieldgroup: stack.some(s => s.includes('fieldgroup')),
        inFormgrid: stack.some(s => s.includes('formgrid')),
      });
    }
    stack.push(kinds);
  }
  return out;
}

// 取 .field 的 id（从 <div class="field..."> 往后到该 field 结束的片段里找第一个 id）
function fieldIds(body) {
  const out = [];
  const re = /<div class="field(?: chk[^"]*)?">([\s\S]*?)(?=<div class="field(?: chk[^"]*)?">|<\/div>\s*<\/div>|<div class="dim"|<fieldset|<\/fieldset>|<div id="setLogFileInfo"|$)/g;
  let m;
  while ((m = re.exec(body))) {
    const id = /id="([^"]+)"/.exec(m[1]);
    if (id) out.push(id[1]);
  }
  return out;
}

// 全部 .field 的 id（全局口径，用于与基线比对）
function allFieldIds(src) {
  const out = [];
  const re = /<div class="field(?: chk[^"]*)?">([\s\S]*?)(?=<div class="field(?: chk[^"]*)?">|<\/fieldset>|<\/div>\s*<\/fieldset>|<div class="dim"|<div id="setLogFileInfo"|$)/g;
  let m;
  while ((m = re.exec(src))) {
    const id = /id="([^"]+)"/.exec(m[1]);
    if (id) out.push(id[1]);
  }
  return out;
}

// 基线：<chk|in> <id>
function baselineIds() {
  return fs.readFileSync(BASELINE, 'utf8').split('\n')
    .map(l => l.trim()).filter(l => l && !l.startsWith('#'))
    .map(l => l.split(/\s+/)[1]);
}

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

const sets = fieldsets(sec);
const named = sets.filter(s => fieldKinds(s.body).length > 0);
const legends = sets.map(s => s.legend).filter(Boolean);
const scanned = scanFields(sec);
const total = scanned.length;

// ---------- A. 分离性：每个组内类型不再交叉 ----------
console.log('\n[A] 每个 fieldset 内复选框与输入框不交叉');
for (const s of named) {
  const k = fieldKinds(s.body);
  const f = flips(k);
  const sep = isSeparated(k);
  ok(sep, '「' + legendText(s) + '」' + (sep ? '已分离' : '仍交叉') + '（翻转 ' + f + ' 次，顺序 = ' + (k.join(' ') || '(空)') + '）');
}
// 至少要有 4 个含字段的组，否则上面的循环可能因为解析失败而"空跑全过"
ok(named.length === 4, '含字段的 fieldset 数量 = ' + named.length + '，期望 4（防止解析失败导致空跑）');

// 逐组锁定 chk/in 数量。期望值 = 本轮重建时的真实结构；
// 这条判据原始形态是"「调度」组 3+3、需要重启组 2+11"，那两组已退役，
// 判据本身（每个含字段的组数量固定）保留并覆盖到现在的四个组。
const wantCounts = [
  ['成长中心自动化', 4, 0],
  ['历史与日志', 0, 2],
  ['网络与容错', 0, 1],
  ['系统提示词', 0, 2],
];
for (const [name, chk, ins] of wantCounts) {
  const s = named.find(x => legendText(x) === name);
  if (!s) { fail++; console.log('  FAIL 找不到含字段的组「' + name + '」'); continue; }
  const k = fieldKinds(s.body);
  const gc = k.filter(x => x === 'CHK').length;
  const gi = k.filter(x => x === 'IN').length;
  ok(gc === chk && gi === ins, '「' + name + '」仍是 ' + chk + ' 复选框 + ' + ins + ' 输入框（实际 ' + gc + '/' + gi + '）');
}

// ---------- B. 分组完整性 ----------
console.log('\n[B] 不存在未包裹在任何容器里的 .field');
ok(total > 0, '解析到 .field 总数 = ' + total + '（>0 才说明解析有效）');
const ungrouped = scanned.filter(f => !f.inFieldgroup && !f.inFormgrid);
const chkOutside = scanned.filter(f => f.kind === 'CHK' && !f.inFieldgroup);
const inOutside = scanned.filter(f => f.kind === 'IN' && !f.inFormgrid);
ok(ungrouped.length === 0, '既不在 .fieldgroup 也不在 .formgrid 内的 .field = ' + ungrouped.length + '（期望 0）');
ok(chkOutside.length === 0, '不在 .fieldgroup 内的复选框 = ' + chkOutside.length + '（期望 0：chk 一律进 checks 网格）');
ok(inOutside.length === 0, '不在 .formgrid 内的输入框 = ' + inOutside.length + '（期望 0：in 一律进两列 formgrid）');
ok(total === 9, '.field 总数 = ' + total + '，期望 9');
for (const s of named) {
  const k = fieldKinds(s.body);
  const need = k.includes('CHK') ? 'fieldgroup' : 'formgrid';
  ok(s.body.includes('class="' + need), '「' + legendText(s) + '」含 .' + need + ' 容器');
}

// ---------- B2. 字面计数复核 ----------
// 与 B 完全独立的两条路径：一个靠 div 栈推包含关系，一个数模板里的字面量。
// 两边总数必须一致 —— 不一致就说明至少有一个解析器错了，
// 而"解析器错"最容易伪装成"实现错"。
console.log('\n[B2] 字面计数复核（与 B 的 div 栈解析互相验证）');
const literalGroups = (sec.match(/<div class="fieldgroup/g) || []).length;
const literalGrids = (sec.match(/<div class="formgrid">/g) || []).length;
const literalChk = (sec.match(/<div class="field chk">/g) || []).length;
const literalIn = (sec.match(/<div class="field">/g) || []).length;
ok(literalGroups === 2, '.fieldgroup 容器数 = ' + literalGroups + '，期望 2（账号池显示过滤的 #setHiddenProviders + 成长中心自动化）');
ok(literalGrids === 3, '.formgrid 容器数 = ' + literalGrids + '，期望 3（历史与日志 / 网络与容错 / 系统提示词）');
ok(literalChk === 4, 'class="field chk" 字面数 = ' + literalChk + '，期望 4（成长组四项）');
ok(literalIn === 5, '<div class="field"> 字面数 = ' + literalIn + '，期望 5（2 + 1 + 2）');
ok(literalChk + literalIn === total, '两条路径的字段总数一致（字面 ' + (literalChk + literalIn) + ' / div 栈 ' + total + '）');
ok(scanned.filter(f => f.kind === 'CHK').length === literalChk, '复选框数量两条路径一致');
ok(sets.length === literalGroups + literalGrids + 3, 'fieldset 数 = ' + sets.length + '（2 个容器组 + 3 个 formgrid 组 + 3 个无字段组 = 8）');

// ---------- C. 零漂移 ----------
console.log('\n[C] 字段 id 零漂移');
const base = baselineIds();
const now = allFieldIds(sec);
ok(base.length === 9, '基线 id 数 = ' + base.length + '，期望 9');
ok(now.length === 9, '当前 id 数 = ' + now.length + '，期望 9');
const missing = base.filter(x => !now.includes(x));
const added = now.filter(x => !base.includes(x));
ok(missing.length === 0, '丢失的 id: ' + (missing.length ? missing.join(', ') : '(无)'));
ok(added.length === 0, '新增的 id: ' + (added.length ? added.join(', ') : '(无)'));
ok(base.slice().sort().join(',') === now.slice().sort().join(','), 'id 集合与基线一致');
// 顺序现在也是契约：本轮基线按**当前 DOM 顺序**落锚。
// 集合相等只挡得住增删字段，挡不住"同名字段被挪到别组"这类静默重排 ——
// 而重排会改变用户看到的设置面板结构，正是这一层要保的东西。
ok(base.join(',') === now.join(','), 'id 顺序与基线逐字一致（本轮基线按 DOM 顺序重建，顺序即契约）');
// 提示词面板的两个字段必须在基线里：新字段若没进基线，以后被删就没人发现。
for (const id of ['setPromptMode', 'setPromptFile']) {
  ok(base.includes(id), '基线包含 ' + id);
}
// label[for] 指向存在的 id
const allIdsInSec = [...sec.matchAll(/<input[^>]*id="([^"]+)"/g)].map(m => m[1]);
const fors = [...sec.matchAll(/<label for="([^"]+)"/g)].map(m => m[1]);
const badFor = fors.filter(f => !allIdsInSec.includes(f));
ok(badFor.length === 0, 'label[for] 指向不存在的 id: ' + (badFor.length ? badFor.join(', ') : '(无)'));
ok(fors.length === 4, 'label[for] 数量 = ' + fors.length + '，期望 4（= 复选框数）');

// ---------- D. 结构保真 ----------
console.log('\n[D] 分组文案与属性保真');
const wantLegends = ['账号池显示过滤', '日常调度', '成长中心自动化', '历史与日志', '网络与容错', '系统提示词', '会话粘性', '运行信息'];
const gotLegends = legends.map(l => l.split('<')[0].trim());
ok(legends.length === 8, 'legend 数量 = ' + legends.length + '，期望 8（组边界未增未减）');
ok(gotLegends.join('|') === wantLegends.join('|'), 'legend 文案与顺序逐字一致：' + gotLegends.join(' / '));
ok(legends.every(l => /<span class="badge/.test(l)), '每个 legend 都带 badge 标记（仅影响显示 / 自动执行 / 立即生效 / 重启生效 / 只读）');
const ro = sets.filter(s => /class="readonly"/.test(s.attrs));
ok(ro.length === 2, '只读 fieldset 数 = ' + ro.length + '，期望 2（会话粘性 / 运行信息）');
ok(ro.every(s => fieldKinds(s.body).length === 0), '只读组不含可编辑 .field');

// 系统提示词（本轮新增的面板）：模式三选一与文件路径是它的全部输入
const promptSet = named.find(s => legendText(s) === '系统提示词');
ok(!!promptSet, '存在「系统提示词」组');
if (promptSet) {
  const opts = [...promptSet.body.matchAll(/<option value="([^"]+)"/g)].map(m => m[1]);
  ok(opts.join(',') === 'custom,append,passthrough', '模式下拉的三选项 = ' + (opts.join(',') || '(空)') + '，期望 custom,append,passthrough');
  ok(/id="setPromptMode"/.test(promptSet.body), '#setPromptMode 保留');
  ok(/id="setPromptFile"[^>]*placeholder="留空 = 使用内置默认"/.test(promptSet.body), '#setPromptFile 的 placeholder 保留');
  ok(promptSet.body.includes('append + 留空文件'), 'append + 空文件的代价说明保留');
  ok(promptSet.body.includes('id="setPromptStatusRO"'), '#setPromptStatusRO 保留');
  ok(promptSet.body.includes('仅对 WorkBuddy 上游生效'), '生效范围说明保留');
}

// 其余组的原文抽查
ok(/<div class="fieldgroup checks" id="setHiddenProviders">/.test(sec), '账号池显示过滤的动态宿主 #setHiddenProviders 保留');
ok(sec.includes('消耗资源 · 默认关（兑换花连登天数、开盲盒花能量、抽奖花次数）'), '成长组的收益/消耗说明保留');
ok(/id="setCheckinLogDays"[^>]*min="1"[^>]*max="3650"/.test(sec), 'setCheckinLogDays 的 min/max 保留');
ok(/id="setReqLogDays"[^>]*min="1"[^>]*max="3650"/.test(sec), 'setReqLogDays 的 min/max 保留');
ok(sec.includes('超过窗口的旧记录在下次裁剪时清除'), 'setReqLogDays 的长 hint 保留');
ok(/id="setUpTimeout"[^>]*min="1"/.test(sec), 'setUpTimeout 的 min 保留');
ok(sec.includes('id="setLogFileInfo"'), '#setLogFileInfo 保留');

// ---------- E. 无 emoji ----------
console.log('\n[E] 风格约定');
const emoji = /[\u{1F300}-\u{1FAFF}\u{2600}-\u{27BF}\u{FE0F}]/u;
ok(!emoji.test(sec), '设置面板无 emoji');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
