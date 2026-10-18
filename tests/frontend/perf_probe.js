// perf_probe.js —— 控制台的性能基线探针（零依赖，只用 Node 22 内置 WebSocket + CDP）。
//
// # 为什么需要它
//
// "性能占用少 / 使用流畅"是本项目的交付目标之一，但此前**没有任何可复现的度量**：
// 所有关于快慢的判断都来自人工体感。体感有两个问题：
//   1. 改前改后无法对比（没有同一把尺子）
//   2. 无法判断"某个优化到底值不值"（比如把 10s 轮询换成 SSE，
//      省下的到底是 0.3% 还是 30% 的 CPU）
//
// 本脚本给出三个可对比的数：
//   load     首屏加载阶段消耗的 CPU 时间与 DOM 节点数（页面重量）
//   idle     **空闲 10 秒**期间的 CPU 占比 —— "什么都不做时它有多吵"
//   panel    切到请求日志面板的渲染成本（最大的一张表）
//
// # 为什么不复用 tests/frontend 的 E2E 设施
//
// 那些脚本 `require('ws')`，而本仓库没有 package.json / node_modules ——
// 裸 clone 直接跑会 `Cannot find module 'ws'`。用 Node 22 的全局 WebSocket
// 可以做到零依赖，这个探针因此在任何机器上都能跑。
//
// # 用法
//
//   node tests/frontend/perf_probe.js http://127.0.0.1:7864/ui
//   node tests/frontend/perf_probe.js http://127.0.0.1:7864/ui --json > before.json
//
// 退出码：0 = 采集成功（数值好坏不由它判断），1 = 采集失败。
// 想对比改造效果：改前跑一次存 JSON，改后再跑一次，比 idle.cpuPercent 与 panel.taskSec。
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const URL_UNDER_TEST = process.argv[2] || 'http://127.0.0.1:7864/ui';
const JSON_OUT = process.argv.includes('--json');
const IDLE_MS = Number(process.env.PERF_IDLE_MS || 10000);
const PORT = Number(process.env.PERF_CDP_PORT || 9333);

const sleep = ms => new Promise(r => setTimeout(r, ms));

function get(url) {
  return new Promise((res, rej) => {
    http.get(url, r => {
      let d = '';
      r.on('data', c => (d += c));
      r.on('end', () => res(d));
    }).on('error', rej);
  });
}

function resolveChrome() {
  try {
    return require('./chrome_path.js').resolveChrome();
  } catch (e) {
    throw new Error('找不到 Chrome/Edge：' + e.message);
  }
}

// 最小 CDP 客户端：只做本脚本需要的事，不引第三方库。
function connect(wsURL) {
  const ws = new WebSocket(wsURL);
  let nextId = 1;
  const pending = new Map();
  const listeners = new Map();

  ws.addEventListener('message', ev => {
    let msg;
    try {
      msg = JSON.parse(ev.data);
    } catch {
      return;
    }
    if (msg.id && pending.has(msg.id)) {
      const { resolve, reject } = pending.get(msg.id);
      pending.delete(msg.id);
      msg.error ? reject(new Error(msg.method + ': ' + JSON.stringify(msg.error))) : resolve(msg.result);
      return;
    }
    if (msg.method && listeners.has(msg.method)) {
      for (const fn of listeners.get(msg.method)) fn(msg.params);
    }
  });

  const ready = new Promise((res, rej) => {
    ws.addEventListener('open', res);
    ws.addEventListener('error', () => rej(new Error('CDP WebSocket 连接失败')));
  });

  return {
    ready,
    send(method, params = {}) {
      const id = nextId++;
      return new Promise((resolve, reject) => {
        pending.set(id, { resolve, reject });
        ws.send(JSON.stringify({ id, method, params }));
        setTimeout(() => {
          if (pending.has(id)) {
            pending.delete(id);
            reject(new Error('CDP 超时: ' + method));
          }
        }, 20000);
      });
    },
    on(method, fn) {
      listeners.set(method, [...(listeners.get(method) || []), fn]);
    },
    once(method) {
      return new Promise(res => {
        const arr = listeners.get(method) || [];
        const fn = p => {
          listeners.set(method, (listeners.get(method) || []).filter(x => x !== fn));
          res(p);
        };
        listeners.set(method, [...arr, fn]);
      });
    },
    close() {
      try {
        ws.close();
      } catch {}
    },
  };
}

function metricsToMap(m) {
  const out = {};
  for (const x of m.metrics || []) out[x.name] = x.value;
  return out;
}

async function main() {
  const chrome = resolveChrome();
  const profile = path.join(os.tmpdir(), 'omni2api-perf-' + Date.now());
  fs.mkdirSync(profile, { recursive: true });

  const proc = spawn(
    chrome,
    [
      '--headless=new',
      '--disable-gpu',
      '--no-first-run',
      '--no-default-browser-check',
      '--disable-extensions',
      '--remote-debugging-port=' + PORT,
      '--user-data-dir=' + profile,
      '--window-size=1440,900',
      'about:blank',
    ],
    { stdio: 'ignore' }
  );

  let cdp;
  try {
    // 等 CDP 端口起来（最多 15s）
    let version = null;
    for (let i = 0; i < 60; i++) {
      try {
        version = JSON.parse(await get('http://127.0.0.1:' + PORT + '/json/version'));
        break;
      } catch {
        await sleep(250);
      }
    }
    if (!version) throw new Error('Chrome 调试端口未就绪（' + PORT + '）');

    // 新建一个 tab 指向目标页（用 /json/new 需要 PUT，旧版是 GET；两种都试）
    let target;
    for (const method of ['PUT', 'GET']) {
      try {
        target = JSON.parse(await requestRaw(method, 'http://127.0.0.1:' + PORT + '/json/new?' + encodeURIComponent(URL_UNDER_TEST)));
        break;
      } catch {}
    }
    if (!target.addr && !target.webSocketDebuggerUrl) throw new Error('/json/new 失败');

    const wsURL = target.webSocketDebuggerUrl || 'ws://127.0.0.1:' + PORT + '/devtools/page/' + target.id;
    cdp = connect(wsURL);
    await cdp.ready;
    await cdp.send('Page.enable');
    await cdp.send('Runtime.enable');
    await cdp.send('Performance.enable');

    // 统计网络请求数**与路径**。为什么要记路径：光有"3 个请求"无法归因 ——
    // 分不清哪些是日志轮询（SSE 能消掉）、哪些是任务历史/统计轮询（消不掉）。
    // 记下路径才能说"这 1 个是 /admin/logs/history，SSE 之后它就是 0"。
    let reqURLs = [];
    cdp.on('Network.requestWillBeSent', p => {
      try {
        const u = new URL(p.request.url);
        reqURLs.push(u.pathname + (u.search || ''));
      } catch {
        reqURLs.push(String(p.request.url));
      }
    });
    try {
      await cdp.send('Network.enable');
    } catch {}

    // 1) 首屏加载
    const loaded = cdp.once('Page.loadEventFired');
    await cdp.send('Page.navigate', { url: URL_UNDER_TEST });
    await Promise.race([loaded, sleep(15000)]);
    await sleep(1200); // 让 refresh() 的首轮请求落定

    const m1 = metricsToMap(await cdp.send('Performance.getMetrics'));
    const load = {
      taskSec: round(m1.TaskDuration),
      domNodes: m1.Nodes,
      jsHeapMB: round((m1.JSHeapUsedSize || 0) / 1048576, 2),
      layoutCount: m1.LayoutCount,
      recalcStyleCount: m1.RecalcStyleCount,
      documents: m1.Documents,
    };

    // 2) 空闲窗口：什么都不做，看它自己有多吵
    const before = metricsToMap(await cdp.send('Performance.getMetrics'));
    await sleep(IDLE_MS);
    const after = metricsToMap(await cdp.send('Performance.getMetrics'));
    const idleTask = (after.TaskDuration || 0) - (before.TaskDuration || 0);
    const idle = {
      windowMs: IDLE_MS,
      taskSec: round(idleTask),
      cpuPercent: round((idleTask / (IDLE_MS / 1000)) * 100, 2),
      longTasks: null, // 需要 PerformanceObserver，见下（页面内注入）
    };

    // 长任务观察：注入一个 PerformanceObserver，统计空闲窗口内 >50ms 的任务
    await cdp.send('Runtime.evaluate', {
      expression: `(() => {
        window.__perfLong = [];
        try {
          new PerformanceObserver(l => { for (const e of l.getEntries()) window.__perfLong.push(e.duration); })
            .observe({ entryTypes: ['longtask'] });
        } catch (e) { window.__perfLong = null; }
        return true;
      })()`,
      returnByValue: true,
    });
    await sleep(IDLE_MS);
    const longRes = await cdp.send('Runtime.evaluate', {
      expression: 'JSON.stringify(window.__perfLong)',
      returnByValue: true,
    });
    try {
      idle.longTasks = JSON.parse(longRes.result.value);
    } catch {
      idle.longTasks = null;
    }

    // 3) 切到「请求日志」面板的渲染成本
    reqURLs = []; // 切面板本身会发请求，先清零再开始计
    const switchBefore = metricsToMap(await cdp.send('Performance.getMetrics'));
    const clicked = await cdp.send('Runtime.evaluate', {
      expression: `(() => {
        const btns = [...document.querySelectorAll('[data-nav], nav button, .nav button')];
        const b = btns.find(x => (x.textContent || '').includes('请求日志'));
        if (!b) return false;
        b.click();
        return true;
      })()`,
      returnByValue: true,
    });
    await sleep(1500); // 等它拉第一页 + 渲染
    const switchAfter = metricsToMap(await cdp.send('Performance.getMetrics'));
    const panel = {
      clicked: !!(clicked.result && clicked.result.value),
      taskSec: round((switchAfter.TaskDuration || 0) - (switchBefore.TaskDuration || 0)),
      domNodesAfter: switchAfter.Nodes,
      requests: reqURLs.length,
      requestPaths: uniq(reqURLs),
    };

    // 4) 停留在日志面板的空闲窗口 —— **这一项才是轮询/推送的真实战场**
    //
    // 默认面板是仪表盘，而 10s 轮询被"面板可见才拉"的 gating 挡住了，
    // 所以上面那个 idle 测不到轮询成本。真正会周期性发请求的场景是
    // 「用户停在请求日志面板上看」。所以这里单独测：
    //   requests     —— 这段窗口内发了几个请求，以及分别是哪些路径
    //   cpuPercent   —— 这些请求 + 重绘带来的 CPU 占比
    // 把日志轮询换成 SSE 之后，requestPaths 里应不再出现 /admin/logs/history
    // 的周期性条目（只剩长连接建立的那一次）。
    reqURLs = [];
    const b2 = metricsToMap(await cdp.send('Performance.getMetrics'));
    await sleep(IDLE_MS);
    const a2 = metricsToMap(await cdp.send('Performance.getMetrics'));
    const idleTask2 = (a2.TaskDuration || 0) - (b2.TaskDuration || 0);
    const idleOnLogs = {
      windowMs: IDLE_MS,
      taskSec: round(idleTask2),
      cpuPercent: round((idleTask2 / (IDLE_MS / 1000)) * 100, 2),
      requests: reqURLs.length,
      requestPaths: uniq(reqURLs),
    };

    const result = {
      url: URL_UNDER_TEST,
      at: new Date().toISOString(),
      chrome: version.Browser,
      load,
      idle,
      panel,
      idleOnLogs,
    };

    if (JSON_OUT) {
      console.log(JSON.stringify(result, null, 2));
    } else {
      console.log('');
      console.log('=== 性能基线 ===');
      console.log('URL            ' + result.url);
      console.log('Chrome         ' + result.chrome);
      console.log('');
      console.log('[load]  首屏       taskSec=' + load.taskSec + 's  DOM=' + load.domNodes +
                  '  JSHeap=' + load.jsHeapMB + 'MB  layout=' + load.layoutCount + '  style=' + load.recalcStyleCount);
      console.log('[idle]  空闲 ' + (IDLE_MS / 1000) + 's    cpu=' + idle.cpuPercent + '%  taskSec=' + idle.taskSec +
                  (idle.longTasks ? '  longTasks=' + (idle.longTasks.length ? idle.longTasks.map(d => Math.round(d)).join(',') : '无') : ''));
      console.log('[panel] 切日志面板  taskSec=' + panel.taskSec + 's  DOM=' + panel.domNodesAfter +
                  '  请求数=' + panel.requests + (panel.clicked ? '' : '  ⚠ 没找到「请求日志」按钮，该项无效'));
      console.log('[idle]  停日志面板  cpu=' + idleOnLogs.cpuPercent + '%  taskSec=' + idleOnLogs.taskSec +
                  '  **请求数=' + idleOnLogs.requests + '**（' + (IDLE_MS / 1000) + 's 窗口）');
      if (idleOnLogs.requestPaths.length) {
        console.log('                     请求路径：' + idleOnLogs.requestPaths.join('  '));
      }
      console.log('');
      console.log('判读：idleOnLogs.requestPaths 决定 SSE 的真实收益 —— 里面出现 /admin/logs/history');
      console.log('      就说明日志在按 10s 轮询（换成 SSE 后这条应消失）；');
      console.log('      剩下的是任务历史/统计等其它轮询，它们不由 SSE 负责。');
      console.log('');
      console.log('判读：idle.cpuPercent 是"什么都不做时的开销"，改造轮询/推送后应显著下降；');
      console.log('      panel.taskSec 是最大一张表的渲染成本，虚拟滚动/增量渲染应让它下降；');
      console.log('      load.domNodes 是页面重量，拆组件或减少隐藏面板的 DOM 会让它下降。');
    }
    return 0;
  } finally {
    if (cdp) cdp.close();
    try {
      proc.kill();
    } catch {}
    await sleep(300);
    try {
      fs.rmSync(profile, { recursive: true, force: true });
    } catch {}
  }
}

function requestRaw(method, url) {
  return new Promise((res, rej) => {
    const u = new URL(url);
    const req = http.request(
      { hostname: u.hostname, port: u.port, path: u.pathname + u.search, method },
      r => {
        let d = '';
        r.on('data', c => (d += c));
        r.on('end', () => res(d));
      }
    );
    req.on('error', rej);
    req.end();
  });
}

function uniq(arr) {
  return [...new Set(arr)];
}

function round(n, digits = 3) {
  if (typeof n !== 'number' || !isFinite(n)) return 0;
  const p = Math.pow(10, digits);
  return Math.round(n * p) / p;
}

main()
  .then(code => process.exit(code))
  .catch(e => {
    console.error('perf_probe 失败：' + (e && e.message));
    process.exit(1);
  });
