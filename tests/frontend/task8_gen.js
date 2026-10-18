
const esc = s => String(s == null ? '' : s)
  .replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
let MANIFEST = { service: 'gateway', providers: [], capabilities: [], admin_routes: [], jobs: [], loaded: false };
let PROVIDERS = [];
let DEFAULT_PROVIDER = '';
let manifestError = '';
function applyManifest(m) {
    m = m && typeof m === 'object' ? m : {};
    // 只保留「非 null 的对象」。数字/字符串/true 都当无效元素丢掉 ——
    // 它们同样会让下游的属性访问出问题，且没有任何合理语义。
    const objs = (v) => (Array.isArray(v) ? v : []).filter(x => x && typeof x === 'object');
    MANIFEST = {
      service: typeof m.service === 'string' && m.service ? m.service : 'gateway',
      // brand 是产品显示名（如 NaiBox），与协议字段 service 分开。
      // ⚠ 必须在这里**显式拷贝**：本函数是按白名单重建 MANIFEST 的，
      // 漏一个键就等于前端永远拿不到它（表现为"后端明明下发了，界面却没变"）。
      // 空串而不是 undefined：让 serviceName() 的 `||` 回落链保持可预测。
      brand: typeof m.brand === 'string' ? m.brand : '',
      providers: objs(m.providers),
      capabilities: objs(m.capabilities),
      admin_routes: objs(m.admin_routes),
      jobs: objs(m.jobs),
      daily_actions: objs(m.daily_actions),
      loaded: true,
    };
    PROVIDERS = MANIFEST.providers;
    // 默认上游从 providers 里找（manifest 顶层没有单独的 default 字段，
    // 上游的 providerInfo.default 才是权威）。找不到就回落第一个。
    const def = PROVIDERS.find(p => p && p.default) || PROVIDERS[0];
    DEFAULT_PROVIDER = def ? def.id : '';
    manifestError = '';
    return MANIFEST;
  }

function providerOf(a) {
    if (!a || typeof a !== 'object') return '';
    return a.provider || DEFAULT_PROVIDER;
  }

function providerRegistryIds() {
    const ids = [];
    for (const p of (PROVIDERS || [])) {
      if (p && typeof p === 'object' && p.id) ids.push(String(p.id));
    }
    // 保持 manifest 下发顺序，**不再字典序重排**。
    //
    // 旧实现 `ids.sort()` 把所有上游拉回字典序（codearts < loomy < trae <
    // workbuddy < workbuddy-intl），把服务端显式排好的展示顺序覆盖掉 ——
    // 账号池分组、模型分组、导航分组全部失效（海外版永远排不到最前）。
    // 顺序的单一来源是服务端（admin/uimanifest.go 的 prioritizeProviders），
    // 这里原样透传；顺序稳定性由服务端的 stable 排序保证。
    return ids;
  }

function providersWithCap(cap) {
    const out = [];
    for (const id of providerRegistryIds()) {
      const p = providerInfo(id);
      if (p && Array.isArray(p.capabilities) && p.capabilities.indexOf(cap) >= 0) {
        out.push(id);
      }
    }
    return out;
  }

function capTitle(cap) {
    for (const c of (MANIFEST.capabilities || [])) {
      if (c && c.id === cap) return c.title || c.id;
    }
    return cap; // 未登记的退回 id 本身，至少还能看出是什么
  }

function providerInfo(id) {
    return (PROVIDERS || []).find(p => p && p.id === id) || null;
  }

function routesFor(providerId, cap) {
    return (MANIFEST.admin_routes || []).filter(r =>
      r && r.provider === providerId &&
      (cap === undefined || r.capability === cap) &&
      String(r.method || '').toUpperCase() === 'GET');
  }

function hasAnyPanel(providerId) {
    const p = providerInfo(providerId);
    if (!p || !Array.isArray(p.capabilities)) return false;
    return p.capabilities.some(c => routesFor(providerId, c).length > 0);
  }

function upstreamGroups() {
    // 过滤掉 null / 非对象元素（CP3 评审 F1）。
    //
    // # 为什么值得加（当前后端确实产不出 null）
    //
    // applyManifest 对四个数组做了 Array.isArray 归一，但**只到数组层、
    // 不到元素层**：providers:[null] 会一路流到这里，让下面
    // `!!x.default` 抛 TypeError。而同一份代码里 applyManifest 的
    // `p && p.default` 是有守卫的 —— 防御不一致本身就是缺陷：
    // 一旦触发，表现为"页面莫名停止刷新"，且 window.__errs 也抓不到。
    //
    // 2 行的成本换掉一类"不可达但真发生时极难查"的故障，值得。
    const out = (PROVIDERS || []).filter(p => p && typeof p === 'object');
    // 没有任何能力面板且账号数为 0 的上游不占导航位置（见计划的"显示门槛"决策）。
    //
    // `p.entry` 是后端下发的「这个上游有没有**任何上号入口**」——
    // 页内登录 / 批量导入 / 凭证重载，三者任一（判据在 admin.hasEntryPoint）。
    //
    // # 它补的是旧门槛探不到的那类上游
    //
    // 旧判据只能看到"有账号"与"有自注册管理端点"。可是**只有批量导入或
    // 凭证重载的上游两者都不成立**：jiyuan（基元律动）的批量导入走通用端点
    // /admin/accounts/import、inkstone（书生·端砚）只有 CredentialLoader。
    // 它们 0 账号时整组从账号池消失 —— 用户看不到分组，也就不知道该往哪放号，
    // 而它们**恰恰是能上号的**。不该从导航消失。
    //
    // 判据仍在后端算：前端不认识任何上游名（判据 1），这里只读那个布尔值。
    // 显示过滤（设置页「账号池显示过滤」）：被勾选隐藏的上游整组不出现。
    //
    // 放在**最后**一步：前面的 hasAnyPanel/entry 判据回答"这个上游值不值得占位置"，
    // 这一层回答"用户明确说不想看到它"。两者的失败方向不同 ——
    // 前者判错会少显示一个能用的上游（本次事故），后者判错是多/少一行界面。
    return out.filter(p => (p.account_count > 0 || hasAnyPanel(p.id) || p.entry)
      && !hiddenProviders.includes(p.id));
  }

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

const M = new Function('esc', 'hiddenProviders',
  "function applyManifest(m) {\n    m = m && typeof m === 'object' ? m : {};\n    // 只保留「非 null 的对象」。数字/字符串/true 都当无效元素丢掉 ——\n    // 它们同样会让下游的属性访问出问题，且没有任何合理语义。\n    const objs = (v) => (Array.isArray(v) ? v : []).filter(x => x && typeof x === 'object');\n    MANIFEST = {\n      service: typeof m.service === 'string' && m.service ? m.service : 'gateway',\n      // brand 是产品显示名（如 NaiBox），与协议字段 service 分开。\n      // ⚠ 必须在这里**显式拷贝**：本函数是按白名单重建 MANIFEST 的，\n      // 漏一个键就等于前端永远拿不到它（表现为\"后端明明下发了，界面却没变\"）。\n      // 空串而不是 undefined：让 serviceName() 的 `||` 回落链保持可预测。\n      brand: typeof m.brand === 'string' ? m.brand : '',\n      providers: objs(m.providers),\n      capabilities: objs(m.capabilities),\n      admin_routes: objs(m.admin_routes),\n      jobs: objs(m.jobs),\n      daily_actions: objs(m.daily_actions),\n      loaded: true,\n    };\n    PROVIDERS = MANIFEST.providers;\n    // 默认上游从 providers 里找（manifest 顶层没有单独的 default 字段，\n    // 上游的 providerInfo.default 才是权威）。找不到就回落第一个。\n    const def = PROVIDERS.find(p => p && p.default) || PROVIDERS[0];\n    DEFAULT_PROVIDER = def ? def.id : '';\n    manifestError = '';\n    return MANIFEST;\n  }\n\nfunction providerOf(a) {\n    if (!a || typeof a !== 'object') return '';\n    return a.provider || DEFAULT_PROVIDER;\n  }\n\nfunction providerRegistryIds() {\n    const ids = [];\n    for (const p of (PROVIDERS || [])) {\n      if (p && typeof p === 'object' && p.id) ids.push(String(p.id));\n    }\n    // 保持 manifest 下发顺序，**不再字典序重排**。\n    //\n    // 旧实现 `ids.sort()` 把所有上游拉回字典序（codearts < loomy < trae <\n    // workbuddy < workbuddy-intl），把服务端显式排好的展示顺序覆盖掉 ——\n    // 账号池分组、模型分组、导航分组全部失效（海外版永远排不到最前）。\n    // 顺序的单一来源是服务端（admin/uimanifest.go 的 prioritizeProviders），\n    // 这里原样透传；顺序稳定性由服务端的 stable 排序保证。\n    return ids;\n  }\n\nfunction providersWithCap(cap) {\n    const out = [];\n    for (const id of providerRegistryIds()) {\n      const p = providerInfo(id);\n      if (p && Array.isArray(p.capabilities) && p.capabilities.indexOf(cap) >= 0) {\n        out.push(id);\n      }\n    }\n    return out;\n  }\n\nfunction capTitle(cap) {\n    for (const c of (MANIFEST.capabilities || [])) {\n      if (c && c.id === cap) return c.title || c.id;\n    }\n    return cap; // 未登记的退回 id 本身，至少还能看出是什么\n  }\n\nfunction providerInfo(id) {\n    return (PROVIDERS || []).find(p => p && p.id === id) || null;\n  }\n\nfunction routesFor(providerId, cap) {\n    return (MANIFEST.admin_routes || []).filter(r =>\n      r && r.provider === providerId &&\n      (cap === undefined || r.capability === cap) &&\n      String(r.method || '').toUpperCase() === 'GET');\n  }\n\nfunction hasAnyPanel(providerId) {\n    const p = providerInfo(providerId);\n    if (!p || !Array.isArray(p.capabilities)) return false;\n    return p.capabilities.some(c => routesFor(providerId, c).length > 0);\n  }\n\nfunction upstreamGroups() {\n    // 过滤掉 null / 非对象元素（CP3 评审 F1）。\n    //\n    // # 为什么值得加（当前后端确实产不出 null）\n    //\n    // applyManifest 对四个数组做了 Array.isArray 归一，但**只到数组层、\n    // 不到元素层**：providers:[null] 会一路流到这里，让下面\n    // `!!x.default` 抛 TypeError。而同一份代码里 applyManifest 的\n    // `p && p.default` 是有守卫的 —— 防御不一致本身就是缺陷：\n    // 一旦触发，表现为\"页面莫名停止刷新\"，且 window.__errs 也抓不到。\n    //\n    // 2 行的成本换掉一类\"不可达但真发生时极难查\"的故障，值得。\n    const out = (PROVIDERS || []).filter(p => p && typeof p === 'object');\n    // 没有任何能力面板且账号数为 0 的上游不占导航位置（见计划的\"显示门槛\"决策）。\n    //\n    // `p.entry` 是后端下发的「这个上游有没有**任何上号入口**」——\n    // 页内登录 / 批量导入 / 凭证重载，三者任一（判据在 admin.hasEntryPoint）。\n    //\n    // # 它补的是旧门槛探不到的那类上游\n    //\n    // 旧判据只能看到\"有账号\"与\"有自注册管理端点\"。可是**只有批量导入或\n    // 凭证重载的上游两者都不成立**：jiyuan（基元律动）的批量导入走通用端点\n    // /admin/accounts/import、inkstone（书生·端砚）只有 CredentialLoader。\n    // 它们 0 账号时整组从账号池消失 —— 用户看不到分组，也就不知道该往哪放号，\n    // 而它们**恰恰是能上号的**。不该从导航消失。\n    //\n    // 判据仍在后端算：前端不认识任何上游名（判据 1），这里只读那个布尔值。\n    // 显示过滤（设置页「账号池显示过滤」）：被勾选隐藏的上游整组不出现。\n    //\n    // 放在**最后**一步：前面的 hasAnyPanel/entry 判据回答\"这个上游值不值得占位置\"，\n    // 这一层回答\"用户明确说不想看到它\"。两者的失败方向不同 ——\n    // 前者判错会少显示一个能用的上游（本次事故），后者判错是多/少一行界面。\n    return out.filter(p => (p.account_count > 0 || hasAnyPanel(p.id) || p.entry)\n      && !hiddenProviders.includes(p.id));\n  }" + '\nreturn { applyManifest, providerOf, providersWithCap, capTitle, providerInfo, routesFor, hasAnyPanel, upstreamGroups,' +
  ' get MANIFEST(){return MANIFEST}, get DEFAULT(){return DEFAULT_PROVIDER} };'
)(esc, []);

// ---------------------------------------------------------------- applyManifest
console.log('\n[1] applyManifest 的形状归一与降级');
M.applyManifest({
  service: 'omni2api',
  providers: [
    { id: 'workbuddy', capabilities: ['chat','models','growth','travel'], default: true, account_count: 3 },
    { id: 'codearts', capabilities: ['chat','models','welfare','quota-probe'], default: false, account_count: 1 },
  ],
  capabilities: [
    { id: 'growth', title: '成长计划' }, { id: 'travel', title: '猫猫旅行' },
    { id: 'welfare', title: '福利中心' }, { id: 'chat', title: '对话' },
    { id: 'models', title: '模型目录' }, { id: 'quota-probe', title: '额度探测' },
  ],
  admin_routes: [
    { provider: 'workbuddy', method: 'GET', path: '/admin/growth', capability: 'growth', title: '成长计划' },
    { provider: 'workbuddy', method: 'POST', path: '/admin/growth/claim', capability: 'growth', title: '领取奖励' },
    { provider: 'workbuddy', method: 'GET', path: '/admin/travel', capability: 'travel', title: '猫猫旅行' },
    { provider: 'codearts', method: 'GET', path: '/admin/welfare', capability: 'welfare', title: '福利中心' },
    { provider: 'codearts', method: 'POST', path: '/admin/welfare/claim', capability: 'welfare', title: '领取福利' },
    { provider: 'codearts', method: 'GET', path: '/admin/subscription', capability: 'welfare', title: '套餐与额度' },
  ],
  jobs: [{ provider: 'codearts', name: 'codearts-refresh', interval_ms: 60000 }],
});
ok(M.MANIFEST.loaded === true, 'applyManifest 后 loaded=true');
ok(M.MANIFEST.service === 'omni2api', 'service 原样存下');
ok(M.DEFAULT === 'workbuddy', '默认上游从 providers[].default 推导（manifest 顶层无 default 字段）');

// 畸形输入必须安全降级（不崩、不产生 half-state）
M.applyManifest(null);
ok(M.MANIFEST.loaded === true && M.MANIFEST.providers.length === 0, 'null 输入 → 空但 loaded');
ok(M.MANIFEST.service === 'gateway', '缺 service → 回落 gateway（不是空串）');
M.applyManifest({ providers: 'not-an-array', capabilities: null, admin_routes: 42 });
ok(Array.isArray(M.MANIFEST.providers) && Array.isArray(M.MANIFEST.admin_routes), '非数组字段被归一成数组（否则 .map 会抛）');
ok(M.MANIFEST.service === 'gateway', '空 service 字符串也回落 gateway');

// 恢复标准 fixture
const FIX = {
  service: 'omni2api',
  providers: [
    { id: 'workbuddy', capabilities: ['chat','models','growth','travel'], default: true, account_count: 3 },
    { id: 'codearts', capabilities: ['chat','models','welfare','quota-probe'], default: false, account_count: 1 },
  ],
  capabilities: [
    { id: 'growth', title: '成长计划' }, { id: 'travel', title: '猫猫旅行' },
    { id: 'welfare', title: '福利中心' }, { id: 'chat', title: '对话' },
    { id: 'models', title: '模型目录' }, { id: 'quota-probe', title: '额度探测' },
  ],
  admin_routes: [
    { provider: 'workbuddy', method: 'GET', path: '/admin/growth', capability: 'growth', title: '成长计划' },
    { provider: 'workbuddy', method: 'POST', path: '/admin/growth/claim', capability: 'growth', title: '领取奖励' },
    { provider: 'workbuddy', method: 'GET', path: '/admin/travel', capability: 'travel', title: '猫猫旅行' },
    { provider: 'codearts', method: 'GET', path: '/admin/welfare', capability: 'welfare', title: '福利中心' },
    { provider: 'codearts', method: 'POST', path: '/admin/welfare/claim', capability: 'welfare', title: '领取福利' },
    { provider: 'codearts', method: 'GET', path: '/admin/subscription', capability: 'welfare', title: '套餐与额度' },
  ],
  jobs: [],
};
M.applyManifest(FIX);

// ---------------------------------------------------------------- 账号归属
console.log('\n[2] 账号的上游归属');
ok(M.providerOf({ uid: 'u1', provider: 'workbuddy' }) === 'workbuddy', '显式带 provider 的账号');
ok(M.providerOf({ uid: 'u2', provider: 'codearts' }) === 'codearts', '另一个上游的账号');
ok(M.providerOf({ uid: 'u3' }) === 'workbuddy', '无 provider 的历史账号 → 归默认上游（向后兼容）');
ok(M.providerOf({ uid: 'u4', provider: '' }) === 'workbuddy', '空串 → 默认上游');
ok(M.providerOf(null) === '', 'null 输入不崩');

// ---------------------------------------------------------------- 能力位查询
console.log('\n[3] 能力位查询（必须从数据读，不能硬编码）');
ok(M.providersWithCap('growth').length === 1, '只有 workbuddy 有 growth');
ok(M.providersWithCap('growth')[0] === 'workbuddy', 'growth 属于 workbuddy');
ok(M.providersWithCap('welfare').length === 1, '只有 codearts 有 welfare');
ok(M.providersWithCap('welfare')[0] === 'codearts', 'welfare 属于 codearts');
ok(M.providersWithCap('chat').length === 2, 'chat 两个上游都有');
ok(M.providersWithCap('nonexistent').length === 0, '不存在的能力返回空');

// 能力位换到别的上游后查询必须跟着变（证明读的是数据）
M.applyManifest(Object.assign({}, FIX, {
  providers: [
    { id: 'workbuddy', capabilities: ['chat'], default: true, account_count: 1 },
    { id: 'third', capabilities: ['growth'], default: false, account_count: 1 },
  ],
}));
ok(M.providersWithCap('growth')[0] === 'third',
  '能力位换到别的上游后，查询跟着变 —— 证明读的是数据而非硬编码');
M.applyManifest(FIX);

// ---------------------------------------------------------------- 能力标题
console.log('\n[4] 能力标题翻译（标题也由后端下发）');
ok(M.capTitle('growth') === '成长计划', 'growth → 成长计划');
ok(M.capTitle('welfare') === '福利中心', 'welfare → 福利中心');
ok(M.capTitle('quota-probe') === '额度探测', 'quota-probe → 额度探测（不是英文 id）');
ok(M.capTitle('unknown-cap') === 'unknown-cap', '未登记的退回 id（至少能看出是什么，不是空白）');

// 标题跟着数据变
M.applyManifest(Object.assign({}, FIX, { capabilities: [{ id: 'growth', title: '改名了' }] }));
ok(M.capTitle('growth') === '改名了', '标题跟着 manifest 变 —— 证明不是硬编码');
M.applyManifest(FIX);

// ---------------------------------------------------------------- 路由查询
console.log('\n[5] 路由查询（只把 GET 当面板入口）');
ok(M.routesFor('workbuddy', 'growth').length === 1,
  'workbuddy 的 growth 面板入口只有 1 个（POST /admin/growth/claim 是动作不是面板）');
ok(M.routesFor('codearts', 'welfare').length === 2,
  'codearts 的 welfare 有 2 个 GET（福利中心 + 套餐与额度）');
ok(M.routesFor('workbuddy').length === 2, 'workbuddy 全部 GET 端点 2 个');
ok(M.routesFor('nosuch').length === 0, '不存在的上游返回空');
M.applyManifest(Object.assign({}, FIX, {
  admin_routes: [{ provider: 'x', method: 'delete', path: '/admin/x', capability: 'chat' }],
}));
ok(M.routesFor('x').length === 0, '小写 delete 也不被当成面板入口（方法比较不区分大小写）');
M.applyManifest(FIX);

// ---------------------------------------------------------------- 上游分组
console.log('\n[6] 上游分组顺序稳定');
const g = M.upstreamGroups();
ok(g.length === 2, '两组，得到 ' + g.length);
ok(g[0].id === 'workbuddy', '默认上游排在最前');
ok(M.upstreamGroups()[0].id === g[0].id, '两次调用顺序一致（稳定）');

// 默认换人 → 顺序跟着换
M.applyManifest(Object.assign({}, FIX, {
  providers: [
    { id: 'codearts', capabilities: ['chat','welfare'], default: true, account_count: 1 },
    { id: 'workbuddy', capabilities: ['chat','growth'], default: false, account_count: 3 },
  ],
}));
ok(M.upstreamGroups()[0].id === 'codearts', '默认上游换成 codearts 后它排最前');
M.applyManifest(FIX);

// 空/null 安全
M.applyManifest({ providers: [], admin_routes: [], capabilities: [] });
ok(M.upstreamGroups().length === 0, '空 providers → 空分组');
ok(M.providerOf({ uid: 'x' }) === '', '无默认上游时 providerOf 返回空串（不崩）');

// ---------------------------------------------------------------- 显示门槛
console.log('\n[7] 分组显示门槛（无面板且无账号的上游不占导航）');
M.applyManifest({
  service: 's', capabilities: [{ id: 'welfare', title: '福利' }],
  providers: [
    { id: 'real', capabilities: ['chat','welfare'], default: true, account_count: 2 },
    { id: 'empty', capabilities: ['chat','models'], default: false, account_count: 0 },
    { id: 'ghost', capabilities: ['chat','models','welfare'], default: false, account_count: 0 },
  ],
  admin_routes: [{ provider: 'ghost', method: 'GET', path: '/admin/w', capability: 'welfare' }],
  jobs: [],
});
const ids = M.upstreamGroups().map(p => p.id);
ok(ids.indexOf('real') >= 0, '有账号的上游保留');
ok(ids.indexOf('empty') < 0, '无账号且只有 chat/models 的上游被隐藏（chat 不构成专属面板）');
ok(ids.indexOf('ghost') >= 0, '无账号但**有专属面板**的上游保留（否则用户看不到它的入口）');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
