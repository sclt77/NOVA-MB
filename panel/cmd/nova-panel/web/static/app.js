/* Nova 面板前端
 *
 * 原生 JS，无框架无构建。理由：
 * 1. 面板界面元素有限（表格、表单、弹窗），框架带来的收益不抵多一层构建链
 * 2. 单文件打包进二进制，部署时只有一个文件
 * 3. 审查方便——没有 node_modules 里几百个间接依赖
 *
 * 安全：所有动态内容都走 textContent 或 esc()，不用 innerHTML 拼用户数据。
 */

'use strict';

// ============================================================ 状态

const state = {
  csrf: '',
  user: '',
  page: 'overview',
  data: {},
};

// ============================================================ 工具

const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => Array.from(document.querySelectorAll(sel));

/** HTML 转义。所有插入 innerHTML 的动态内容都必须过这个。 */
function esc(s) {
  if (s === null || s === undefined) return '';
  return String(s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

function fmtBytes(n) {
  if (!n || n <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return v.toFixed(v >= 100 || i === 0 ? 0 : 1) + ' ' + units[i];
}

function fmtTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d)) return esc(iso);
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ` +
         `${p(d.getHours())}:${p(d.getMinutes())}`;
}

function toast(msg, kind = '') {
  const el = $('#toast');
  el.textContent = msg;
  el.className = 'toast ' + kind;
  clearTimeout(el._timer);
  el._timer = setTimeout(() => el.classList.add('hidden'), 3200);
}

// ============================================================ API

async function api(method, path, body) {
  const opts = {
    method,
    headers: { 'Accept': 'application/json' },
    credentials: 'same-origin',
  };

  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }

  // 非 GET 要带 CSRF
  if (method !== 'GET' && state.csrf) {
    opts.headers['X-CSRF-Token'] = state.csrf;
  }

  let res;
  try {
    res = await fetch(path, opts);
  } catch (e) {
    throw new Error('网络请求失败，请检查面板是否在运行');
  }

  let data;
  try {
    data = await res.json();
  } catch (e) {
    throw new Error(`服务器返回异常（HTTP ${res.status}）`);
  }

  if (res.status === 401) {
    showLogin();
    throw new Error(data.error || '登录已过期');
  }

  if (!data.ok) {
    throw new Error(data.error || '操作失败');
  }

  return data.data;
}

async function uploadFile(path, file) {
  const form = new FormData();
  form.append('package', file);

  const res = await fetch(path, {
    method: 'POST',
    headers: { 'X-CSRF-Token': state.csrf },
    credentials: 'same-origin',
    body: form,
  });

  const data = await res.json().catch(() => ({ ok: false, error: '响应异常' }));

  if (res.status === 401) {
    showLogin();
    throw new Error('登录已过期');
  }
  if (!data.ok) throw new Error(data.error || '上传失败');

  return data.data;
}

// ============================================================ 弹窗

function showModal(title, ok, output, durationMs) {
  $('#modal-title').textContent = title;

  const status = $('#modal-status');
  if (ok) {
    status.innerHTML = '<span class="dot on"></span><span style="color:var(--success)">执行成功</span>';
  } else {
    status.innerHTML = '<span class="dot err"></span><span style="color:var(--danger)">执行失败</span>';
  }

  if (durationMs) {
    status.innerHTML += `<span class="muted" style="font-weight:400;font-size:12px">（耗时 ${(durationMs/1000).toFixed(1)}s）</span>`;
  }

  $('#modal-output').textContent = output || '（无输出）';
  $('#modal').classList.remove('hidden');
}

$('#modal-close').onclick = () => $('#modal').classList.add('hidden');
$('#modal-ok').onclick = () => $('#modal').classList.add('hidden');
$('#modal').onclick = (e) => { if (e.target.id === 'modal') $('#modal').classList.add('hidden'); };

/** 确认弹窗。requireText 非空时必须输入匹配内容才能确认。 */
function confirmDialog({ title, text, okText = '确认', requireText = '', danger = true }) {
  return new Promise((resolve) => {
    $('#confirm-title').textContent = title;
    $('#confirm-text').textContent = text;

    const okBtn = $('#confirm-ok');
    const inputWrap = $('#confirm-input-wrap');
    const input = $('#confirm-input');

    okBtn.textContent = okText;
    okBtn.className = 'btn ' + (danger ? 'btn-danger' : 'btn-primary');

    if (requireText) {
      inputWrap.classList.remove('hidden');
      $('#confirm-input-label').textContent = `请输入「${requireText}」确认：`;
      input.value = '';
      okBtn.disabled = true;
      input.oninput = () => { okBtn.disabled = input.value.trim() !== requireText; };
    } else {
      inputWrap.classList.add('hidden');
      okBtn.disabled = false;
    }

    const done = (v) => {
      $('#confirm').classList.add('hidden');
      okBtn.onclick = null;
      $('#confirm-cancel').onclick = null;
      resolve(v);
    };

    okBtn.onclick = () => done(true);
    $('#confirm-cancel').onclick = () => done(false);
    $('#confirm').classList.remove('hidden');

    if (requireText) input.focus();
  });
}

// ============================================================ 登录

function showLogin() {
  $('#login-view').classList.remove('hidden');
  $('#app-view').classList.add('hidden');
  $('#login-user').focus();
}

function showApp() {
  $('#login-view').classList.add('hidden');
  $('#app-view').classList.remove('hidden');
  $('#current-user').textContent = state.user;
}

$('#login-form').onsubmit = async (e) => {
  e.preventDefault();

  const btn = $('#login-btn');
  const errEl = $('#login-error');
  errEl.classList.add('hidden');

  btn.disabled = true;
  btn.textContent = '登录中…';

  try {
    const data = await api('POST', '/api/login', {
      username: $('#login-user').value.trim(),
      password: $('#login-pass').value,
    });

    state.csrf = data.csrfToken;
    state.user = data.username;

    $('#login-pass').value = '';
    showApp();
    navigate('overview');
  } catch (err) {
    errEl.textContent = err.message;
    errEl.classList.remove('hidden');
  } finally {
    btn.disabled = false;
    btn.textContent = '登录';
  }
};

$('#logout-btn').onclick = async () => {
  try { await api('POST', '/api/logout'); } catch (e) { /* 忽略 */ }
  state.csrf = '';
  showLogin();
};

// ============================================================ 路由

const PAGES = {
  overview: { title: '概览', sub: '服务器与服务状态', render: renderOverview, load: loadOverview },
  deploy:   { title: '部署管理', sub: '上传部署包、版本切换与回滚', render: renderDeploy, load: loadDeploy },
  backup:   { title: '备份恢复', sub: '数据库与上传文件的备份', render: renderBackup, load: loadBackup },
  domain:   { title: '域名证书', sub: '域名绑定与 SSL 自动续期', render: renderDomain, load: loadDomain },
  database: { title: '数据库', sub: '表结构与容量（只读）', render: renderDatabase, load: loadDatabase },
  cron:     { title: '定时任务', sub: '备份、健康检查与证书续期', render: renderCron, load: loadCron },
  logs:     { title: '日志', sub: '应用与 Nginx 日志', render: renderLogs, load: loadLogs },
  audit:    { title: '操作记录', sub: '所有写操作的审计日志', render: renderAudit, load: loadAudit },
  settings: { title: '设置', sub: '账号与安全', render: renderSettings, load: null },
};

function navigate(page) {
  state.page = page;

  $$('.nav-item').forEach((el) => {
    el.classList.toggle('active', el.dataset.page === page);
  });

  const p = PAGES[page];
  $('#page-title').textContent = p.title;
  $('#page-sub').textContent = p.sub;

  $('#page-content').innerHTML = '<div class="loading">加载中…</div>';

  if (p.load) {
    p.load().catch((e) => {
      $('#page-content').innerHTML =
        `<div class="empty"><div class="big">⚠</div><p>${esc(e.message)}</p></div>`;
    });
  } else {
    p.render();
  }
}

$('#nav').onclick = (e) => {
  const item = e.target.closest('.nav-item');
  if (item) navigate(item.dataset.page);
};

$('#refresh-btn').onclick = () => navigate(state.page);

// ============================================================ 概览

async function loadOverview() {
  const ov = await api('GET', '/api/overview');
  state.data.overview = ov;
  renderOverview();
}

function renderOverview() {
  const ov = state.data.overview;
  if (!ov) return;

  // 顶部告警
  const badge = $('#alert-badge');
  if (ov.warnings && ov.warnings.length) {
    badge.textContent = `${ov.warnings.length} 项告警`;
    badge.classList.remove('hidden');
  } else {
    badge.classList.add('hidden');
  }

  const memCls = ov.memPercent >= 90 ? 'crit' : ov.memPercent >= 75 ? 'warn' : 'ok';
  const diskCls = ov.diskCrit ? 'crit' : ov.diskWarn ? 'warn' : 'ok';

  let html = '';

  // 告警条
  if (ov.warnings && ov.warnings.length) {
    html += `<div class="card" style="border-color:var(--warning);margin-bottom:14px">
      <div class="card-head" style="margin-bottom:8px">
        <h3 style="color:var(--warning)">⚠ 需要注意</h3>
      </div>
      ${ov.warnings.map((w) => `<div class="hint warn" style="margin-top:4px">· ${esc(w)}</div>`).join('')}
    </div>`;
  }

  // 资源指标
  html += `<div class="grid grid-4" style="margin-bottom:14px">
    <div class="stat">
      <div class="stat-label">内存</div>
      <div class="stat-value">${ov.memPercent}%</div>
      <div class="stat-hint">${ov.memUsedMB} / ${ov.memTotalMB} MB</div>
      <div class="bar"><div class="bar-fill ${memCls}" style="width:${ov.memPercent}%"></div></div>
    </div>
    <div class="stat">
      <div class="stat-label">磁盘 /var</div>
      <div class="stat-value">${ov.diskPercent}%</div>
      <div class="stat-hint">${fmtBytes(ov.diskUsedMB * 1048576)} / ${fmtBytes(ov.diskTotalMB * 1048576)}</div>
      <div class="bar"><div class="bar-fill ${diskCls}" style="width:${ov.diskPercent}%"></div></div>
    </div>
    <div class="stat">
      <div class="stat-label">CPU</div>
      <div class="stat-value">${ov.cpuCores} 核</div>
      <div class="stat-hint">负载 ${(ov.loadAvg || []).map((v) => v.toFixed(2)).join(' / ')}</div>
    </div>
    <div class="stat">
      <div class="stat-label">运行时长</div>
      <div class="stat-value" style="font-size:16px">${esc(ov.uptime)}</div>
      <div class="stat-hint">${esc(ov.hostname)}</div>
    </div>
  </div>`;

  // 服务状态
  html += `<div class="card" style="margin-bottom:14px">
    <div class="card-head">
      <div>
        <h3>服务状态</h3>
        <div class="card-sub">启停会立即生效</div>
      </div>
    </div>
    <table class="table">
      <thead><tr>
        <th>服务</th><th>状态</th><th>开机自启</th><th style="text-align:right">操作</th>
      </tr></thead>
      <tbody>`;

  for (const svc of ov.services) {
    const dotCls = svc.active ? 'on' : 'off';
    const stateText = svc.active ? '运行中' : (svc.state === 'inactive' ? '已停止' : esc(svc.state));

    html += `<tr>
      <td><span class="dot ${dotCls}"></span>${esc(svc.label)}
        <span class="muted" style="font-size:11.5px">${esc(svc.name)}</span></td>
      <td><span class="badge ${svc.active ? 'ok' : 'plain'}">${stateText}</span></td>
      <td class="muted" style="font-size:12px">${svc.enabled ? '是' : '否'}</td>
      <td style="text-align:right">
        ${svc.active
          ? `<button class="btn btn-sm" data-svc="restart" data-name="${esc(svc.name)}">重启</button>
             <button class="btn btn-sm" data-svc="stop" data-name="${esc(svc.name)}">停止</button>`
          : `<button class="btn btn-sm btn-primary" data-svc="start" data-name="${esc(svc.name)}">启动</button>`}
      </td>
    </tr>`;
  }

  html += `</tbody></table></div>`;

  // 系统信息
  html += `<div class="card">
    <div class="card-head"><h3>系统信息</h3></div>
    <table class="table">
      <tr><td class="muted" style="width:110px">操作系统</td><td>${esc(ov.os || '未知')}</td></tr>
      <tr><td class="muted">内核</td><td class="muted" style="font-family:var(--mono);font-size:12px">${esc(ov.kernel || '未知')}</td></tr>
      <tr><td class="muted">服务器时间</td><td>${esc(ov.serverTime)}</td></tr>
      <tr><td class="muted">面板版本</td><td>${esc(ov.version)} · ${esc(ov.goVersion)}</td></tr>
    </table>
  </div>`;

  $('#page-content').innerHTML = html;

  // 绑定服务操作
  $('#page-content').querySelectorAll('[data-svc]').forEach((btn) => {
    btn.onclick = () => serviceAction(btn.dataset.svc, btn.dataset.name, btn);
  });
}

async function serviceAction(action, service, btn) {
  const labels = { start: '启动', stop: '停止', restart: '重启' };

  if (action === 'stop' || action === 'restart') {
    const ok = await confirmDialog({
      title: `${labels[action]}服务`,
      text: `确认${labels[action]}「${service}」？${action === 'stop' ? '停止后相关功能将不可用。' : ''}`,
      okText: labels[action],
    });
    if (!ok) return;
  }

  btn.disabled = true;
  const old = btn.textContent;
  btn.textContent = '处理中…';

  try {
    const res = await api('POST', `/api/service/${action}`, { service });
    if (res.ok) {
      toast(`${labels[action]}成功`, 'ok');
      await loadOverview();
    } else {
      showModal(`${labels[action]}失败`, false, res.output, res.durationMs);
    }
  } catch (e) {
    toast(e.message, 'err');
  } finally {
    btn.disabled = false;
    btn.textContent = old;
  }
}

// ============================================================ 部署

async function loadDeploy() {
  const releases = await api('GET', '/api/deploy/releases');
  state.data.releases = releases;
  renderDeploy();
}

function renderDeploy() {
  const releases = state.data.releases || [];

  let html = `
  <div class="card" style="margin-bottom:14px">
    <div class="card-head">
      <div>
        <h3>上传部署包</h3>
        <div class="card-sub">包内需包含 nova-api.jar 与 web/index.html</div>
      </div>
    </div>
    <div class="upload-zone" id="upload-zone">
      <div class="big">▲</div>
      <p>点击选择文件，或拖拽到这里</p>
      <small>支持 zip，最大 500MB</small>
      <input type="file" id="file-input" accept=".zip" class="hidden">
    </div>
    <div class="hint">
      部署流程：校验 → 备份数据库 → 解压 → 切换版本 → 健康检查 → 失败自动回滚。
      历史版本保留最近 5 个，可随时回滚。
    </div>
  </div>

  <div class="card">
    <div class="card-head">
      <div>
        <h3>历史版本</h3>
        <div class="card-sub">共 ${releases.length} 个版本</div>
      </div>
      <button class="btn btn-sm" id="rollback-btn">回滚到上一版</button>
    </div>`;

  if (!releases.length) {
    html += `<div class="empty"><div class="big">▤</div><p>还没有部署记录</p></div>`;
  } else {
    html += `<table class="table">
      <thead><tr>
        <th>版本</th><th>部署时间</th><th class="num">包大小</th>
        <th>前端</th><th>状态</th>
      </tr></thead>
      <tbody>`;

    for (const r of releases) {
      html += `<tr>
        <td style="font-family:var(--mono);font-size:12.5px">${esc(r.name)}</td>
        <td class="muted" style="font-size:12px">${fmtTime(r.createdAt)}</td>
        <td class="num muted">${fmtBytes(r.size)}</td>
        <td>${r.hasWeb ? '<span class="badge info">含</span>' : '<span class="badge plain">无</span>'}</td>
        <td>${r.current ? '<span class="badge ok">当前版本</span>' : '<span class="badge plain">历史</span>'}</td>
      </tr>`;
    }

    html += `</tbody></table>`;
  }

  html += `</div>`;

  $('#page-content').innerHTML = html;

  // 上传
  const zone = $('#upload-zone');
  const input = $('#file-input');

  zone.onclick = () => input.click();
  input.onchange = () => {
    if (input.files.length) doUpload(input.files[0]);
  };

  zone.ondragover = (e) => { e.preventDefault(); zone.classList.add('drag'); };
  zone.ondragleave = () => zone.classList.remove('drag');
  zone.ondrop = (e) => {
    e.preventDefault();
    zone.classList.remove('drag');
    if (e.dataTransfer.files.length) doUpload(e.dataTransfer.files[0]);
  };

  // 回滚
  const rb = $('#rollback-btn');
  if (rb) rb.onclick = doRollback;
}

async function doUpload(file) {
  if (!file.name.endsWith('.zip')) {
    toast('只支持 zip 格式', 'err');
    return;
  }

  const zone = $('#upload-zone');
  zone.innerHTML = `<div class="big">◌</div><p>上传中…</p><small>${esc(file.name)}（${fmtBytes(file.size)}）</small>`;

  try {
    const res = await uploadFile('/api/deploy/upload', file);

    // 校验通过，确认部署
    let msg = `文件：${file.name}\n大小：${fmtBytes(res.size)}\n\n`;
    if (res.warnings && res.warnings.length) {
      msg += '注意：\n' + res.warnings.join('\n') + '\n\n';
    }
    msg += '部署过程中服务会短暂中断，完成后自动做健康检查。';

    const ok = await confirmDialog({
      title: '部署到服务器',
      text: msg,
      okText: '开始部署',
      danger: false,
    });

    if (!ok) {
      renderDeploy();
      return;
    }

    zone.innerHTML = `<div class="big">◌</div><p>部署中，请稍候…</p><small>正在备份、解压、重启服务</small>`;

    const result = await api('POST', '/api/deploy/run', { path: res.path });

    showModal('部署结果', result.ok, result.output, result.durationMs);
    await loadDeploy();

  } catch (e) {
    toast(e.message, 'err');
    await loadDeploy();
  }
}

async function doRollback() {
  const ok = await confirmDialog({
    title: '回滚到上一版本',
    text: '将把当前版本切回上一个部署的版本，并重启服务。当前版本会保留在历史记录里。',
    okText: '回滚',
  });
  if (!ok) return;

  try {
    const res = await api('POST', '/api/deploy/rollback');
    showModal('回滚结果', res.ok, res.output, res.durationMs);
    await loadDeploy();
  } catch (e) {
    toast(e.message, 'err');
  }
}

// ============================================================ 备份

async function loadBackup() {
  const data = await api('GET', '/api/backups');
  state.data.backups = data;
  renderBackup();
}

function renderBackup() {
  const data = state.data.backups || { items: [], totalSize: 0 };
  const items = data.items || [];

  let html = `
  <div class="grid grid-3" style="margin-bottom:14px">
    <div class="stat">
      <div class="stat-label">备份数量</div>
      <div class="stat-value">${items.length}</div>
      <div class="stat-hint">日备 7 · 周备 4 · 月备 3</div>
    </div>
    <div class="stat">
      <div class="stat-label">占用空间</div>
      <div class="stat-value">${fmtBytes(data.totalSize)}</div>
    </div>
    <div class="stat">
      <div class="stat-label">最近备份</div>
      <div class="stat-value" style="font-size:15px">${items.length ? esc(items[0].stamp) : '无'}</div>
    </div>
  </div>

  <div class="card" style="margin-bottom:14px">
    <div class="card-head">
      <div>
        <h3>立即备份</h3>
        <div class="card-sub">全量备份包含数据库与上传文件</div>
      </div>
      <div style="display:flex;gap:8px">
        <button class="btn btn-sm" id="backup-quick">仅数据库</button>
        <button class="btn btn-sm btn-primary" id="backup-full">全量备份</button>
      </div>
    </div>
    <div class="hint">
      备份文件存在 <code style="font-family:var(--mono);font-size:11.5px">/var/lib/nova/backup</code>。
      建议同时开启异地备份（同步到对象存储），否则磁盘损坏会连备份一起丢。
    </div>
  </div>

  <div class="card">
    <div class="card-head">
      <div>
        <h3>备份列表</h3>
        <div class="card-sub">点击「验证」会把备份导入临时库，确认真的可用</div>
      </div>
      <button class="btn btn-sm" id="verify-btn">验证最新备份</button>
    </div>`;

  if (!items.length) {
    html += `<div class="empty"><div class="big">▣</div><p>还没有备份</p></div>`;
  } else {
    html += `<table class="table">
      <thead><tr>
        <th>时间</th><th class="num">数据库</th><th class="num">上传文件</th>
        <th style="text-align:right">操作</th>
      </tr></thead>
      <tbody>`;

    for (const b of items) {
      html += `<tr>
        <td style="font-family:var(--mono);font-size:12.5px">${esc(b.stamp)}</td>
        <td class="num muted">${fmtBytes(b.dbSize)}</td>
        <td class="num muted">${b.hasUpload ? fmtBytes(b.uploads) : '—'}</td>
        <td style="text-align:right">
          <button class="btn btn-sm" data-restore="${esc(b.stamp)}">恢复</button>
          <button class="btn btn-sm" data-del="${esc(b.stamp)}" style="color:var(--danger)">删除</button>
        </td>
      </tr>`;
    }

    html += `</tbody></table>`;
  }

  html += `</div>`;

  $('#page-content').innerHTML = html;

  $('#backup-quick').onclick = () => runBackup(false);
  $('#backup-full').onclick = () => runBackup(true);
  $('#verify-btn').onclick = verifyBackup;

  $('#page-content').querySelectorAll('[data-restore]').forEach((btn) => {
    btn.onclick = () => doRestore(btn.dataset.restore);
  });
  $('#page-content').querySelectorAll('[data-del]').forEach((btn) => {
    btn.onclick = () => doDeleteBackup(btn.dataset.del);
  });
}

async function runBackup(full) {
  const ok = await confirmDialog({
    title: full ? '全量备份' : '备份数据库',
    text: full
      ? '将备份数据库和所有上传文件，数据量大时可能需要几分钟。'
      : '只备份数据库，速度快，适合部署前临时备份。',
    okText: '开始备份',
    danger: false,
  });
  if (!ok) return;

  toast('备份中，请稍候…');
  try {
    const res = await api('POST', '/api/backups/run', { full });
    showModal('备份结果', res.ok, res.output, res.durationMs);
    await loadBackup();
  } catch (e) {
    toast(e.message, 'err');
  }
}

async function verifyBackup() {
  toast('验证中，需要导入到临时库，请稍候…');
  try {
    const res = await api('POST', '/api/backups/verify');
    showModal('备份验证', res.ok, res.output, res.durationMs);
  } catch (e) {
    toast(e.message, 'err');
  }
}

async function doRestore(stamp) {
  const ok = await confirmDialog({
    title: '恢复备份',
    text: `将用 ${stamp} 的备份覆盖当前数据。\n\n恢复前会自动备份当前状态，但仍建议谨慎操作。`,
    okText: '确认恢复',
    requireText: stamp,
  });
  if (!ok) return;

  toast('恢复中，请稍候…');
  try {
    const res = await api('POST', '/api/backups/restore', { stamp, scope: 'all' });
    showModal('恢复结果', res.ok, res.output, res.durationMs);
    await loadBackup();
  } catch (e) {
    toast(e.message, 'err');
  }
}

async function doDeleteBackup(stamp) {
  const ok = await confirmDialog({
    title: '删除备份',
    text: `确认删除 ${stamp} 的备份？删除后不可恢复。`,
    okText: '删除',
  });
  if (!ok) return;

  try {
    await api('DELETE', `/api/backups/${stamp}`);
    toast('已删除', 'ok');
    await loadBackup();
  } catch (e) {
    toast(e.message, 'err');
  }
}

// ============================================================ 域名

async function loadDomain() {
  const d = await api('GET', '/api/domain');
  state.data.domain = d;
  renderDomain();
}

function renderDomain() {
  const d = state.data.domain || {};
  const domain = d.domain === '_' ? '' : d.domain;

  let certBadge = '<span class="badge plain">未配置</span>';
  if (d.hasCert) {
    const cls = d.certDays < 14 ? 'err' : d.certDays < 30 ? 'warn' : 'ok';
    certBadge = `<span class="badge ${cls}">剩余 ${d.certDays} 天</span>`;
  }

  let html = `
  <div class="card" style="margin-bottom:14px">
    <div class="card-head">
      <div>
        <h3>域名绑定</h3>
        <div class="card-sub">绑定后 Nginx 会按该域名提供站点</div>
      </div>
    </div>
    <div class="form-row">
      <label class="field">
        <span>域名</span>
        <input type="text" id="domain-input" placeholder="example.com"
               value="${esc(domain)}">
      </label>
      <button class="btn btn-primary" id="domain-save">保存并生效</button>
    </div>
    <div class="hint">
      绑定前请先在 DNS 服务商处添加 A 记录指向本机 IP。<br>
      绑定会自动更新后端的对外地址（邮件里的重置链接按它拼）。
    </div>
  </div>

  <div class="card" style="margin-bottom:14px">
    <div class="card-head">
      <div>
        <h3>SSL 证书</h3>
        <div class="card-sub">Let's Encrypt 免费证书，90 天有效，自动续期</div>
      </div>
      ${certBadge}
    </div>
    <div class="form-row">
      <label class="field">
        <span>邮箱（接收到期提醒，可留空）</span>
        <input type="email" id="cert-email" placeholder="you@example.com">
      </label>
      <button class="btn btn-primary" id="cert-issue">申请证书</button>
      <button class="btn" id="cert-renew">手动续期</button>
    </div>
    <div class="hint">
      申请前必须：① 域名已解析到本机；② 80 端口可从公网访问（Let's Encrypt 要验证域名所有权）。<br>
      申请成功后会自动配置 HTTPS 并把 HTTP 请求跳转到 HTTPS。
    </div>
  </div>

  <div class="card">
    <div class="card-head"><h3>当前配置</h3></div>
    <table class="table">
      <tr><td class="muted" style="width:130px">绑定域名</td>
          <td>${domain ? esc(domain) : '<span class="muted">未绑定（任意域名可访问）</span>'}</td></tr>
      <tr><td class="muted">HTTPS</td>
          <td>${d.ssl ? '<span class="badge ok">已启用</span>' : '<span class="badge plain">未启用</span>'}</td></tr>
      <tr><td class="muted">证书路径</td>
          <td class="muted" style="font-family:var(--mono);font-size:11.5px">${esc(d.certPath || '—')}</td></tr>
    </table>
  </div>`;

  $('#page-content').innerHTML = html;

  $('#domain-save').onclick = async () => {
    const v = $('#domain-input').value.trim();
    if (!v) { toast('请输入域名', 'err'); return; }

    const ok = await confirmDialog({
      title: '绑定域名',
      text: `将把站点绑定到 ${v}，并重载 Nginx。绑定前请确认 DNS 已解析到本机。`,
      okText: '绑定',
      danger: false,
    });
    if (!ok) return;

    try {
      await api('POST', '/api/domain', { domain: v });
      toast('域名已绑定', 'ok');
      await loadDomain();
    } catch (e) {
      toast(e.message, 'err');
    }
  };

  $('#cert-issue').onclick = async () => {
    const v = $('#domain-input').value.trim();
    const email = $('#cert-email').value.trim();

    if (!v) { toast('请先填写域名', 'err'); return; }

    const ok = await confirmDialog({
      title: '申请 SSL 证书',
      text: `将为 ${v} 申请 Let's Encrypt 证书。\n\n` +
            '前提：域名已解析到本机，且 80 端口可从公网访问。\n' +
            '申请过程约 30 秒到 2 分钟。',
      okText: '开始申请',
      danger: false,
    });
    if (!ok) return;

    toast('正在申请证书…');
    try {
      const res = await api('POST', '/api/domain/cert', { domain: v, email });
      showModal('证书申请结果', res.ok, res.output, res.durationMs);
      await loadDomain();
    } catch (e) {
      toast(e.message, 'err');
    }
  };

  $('#cert-renew').onclick = async () => {
    toast('续期中…');
    try {
      const res = await api('POST', '/api/domain/cert/renew');
      showModal('续期结果', res.ok, res.output, res.durationMs);
      await loadDomain();
    } catch (e) {
      toast(e.message, 'err');
    }
  };
}

// ============================================================ 数据库

async function loadDatabase() {
  const d = await api('GET', '/api/database');
  state.data.database = d;
  renderDatabase();
}

function renderDatabase() {
  const d = state.data.database || {};

  if (!d.connected) {
    $('#page-content').innerHTML = `
      <div class="card">
        <div class="card-head"><h3>数据库</h3></div>
        <div class="empty">
          <div class="big">▤</div>
          <p>${esc(d.error || '无法连接数据库')}</p>
        </div>
      </div>`;
    return;
  }

  let html = `
  <div class="grid grid-4" style="margin-bottom:14px">
    <div class="stat">
      <div class="stat-label">版本</div>
      <div class="stat-value" style="font-size:15px">${esc(d.version)}</div>
    </div>
    <div class="stat">
      <div class="stat-label">数据库</div>
      <div class="stat-value" style="font-size:15px">${esc(d.database)}</div>
    </div>
    <div class="stat">
      <div class="stat-label">总大小</div>
      <div class="stat-value">${d.sizeMB} <span style="font-size:13px;font-weight:400">MB</span></div>
    </div>
    <div class="stat">
      <div class="stat-label">连接数</div>
      <div class="stat-value">${d.connections} <span style="font-size:13px;font-weight:400">/ ${d.maxConns}</span></div>
    </div>
  </div>

  <div class="card">
    <div class="card-head">
      <div>
        <h3>数据表</h3>
        <div class="card-sub">共 ${(d.tables || []).length} 张表 · 运行 ${esc(d.uptime || '—')}</div>
      </div>
    </div>
    <div class="hint" style="margin-bottom:12px">
      面板不提供 SQL 控制台。需要执行 SQL 请用命令行：
      <code style="font-family:var(--mono);font-size:11.5px">mysql --defaults-file=/etc/nova/.mysql-root.cnf nova</code>
    </div>
    <table class="table">
      <thead><tr>
        <th>表名</th><th class="num">行数</th><th class="num">数据</th>
        <th class="num">索引</th><th class="num">合计</th>
      </tr></thead>
      <tbody>`;

  for (const t of d.tables || []) {
    html += `<tr>
      <td style="font-family:var(--mono);font-size:12.5px">${esc(t.name)}</td>
      <td class="num muted">${t.rows.toLocaleString()}</td>
      <td class="num muted">${t.dataMB} MB</td>
      <td class="num muted">${t.indexMB} MB</td>
      <td class="num">${t.sizeMB} MB</td>
    </tr>`;
  }

  html += `</tbody></table></div>`;

  $('#page-content').innerHTML = html;
}

// ============================================================ 定时任务

async function loadCron() {
  const tasks = await api('GET', '/api/cron');
  state.data.cron = tasks;
  renderCron();
}

function renderCron() {
  const tasks = state.data.cron || [];

  let html = `
  <div class="card" style="margin-bottom:14px">
    <div class="card-head">
      <div>
        <h3>定时任务</h3>
        <div class="card-sub">面板只管理预定义任务，不支持自由填写命令</div>
      </div>
      <div style="display:flex;gap:8px">
        <button class="btn btn-sm" id="cron-remove">移除 Nova 任务</button>
        <button class="btn btn-sm btn-primary" id="cron-install">安装默认任务</button>
      </div>
    </div>
    <div class="hint">
      默认任务：每天凌晨 3 点全量备份 · 每周日凌晨 4 点验证备份 · 每 10 分钟健康检查。
    </div>
  </div>

  <div class="card">
    <div class="card-head"><h3>当前任务</h3></div>`;

  if (!tasks.length) {
    html += `<div class="empty"><div class="big">◷</div><p>还没有定时任务，点击「安装默认任务」</p></div>`;
  } else {
    html += `<table class="table">
      <thead><tr><th>执行时间</th><th>命令</th><th>来源</th></tr></thead>
      <tbody>`;

    for (const t of tasks) {
      html += `<tr>
        <td style="font-family:var(--mono);font-size:12px">${esc(t.schedule)}</td>
        <td style="font-family:var(--mono);font-size:11.5px;word-break:break-all">${esc(t.command)}</td>
        <td>${t.managed ? '<span class="badge info">面板管理</span>' : '<span class="badge plain">手动添加</span>'}</td>
      </tr>`;
    }

    html += `</tbody></table>`;
  }

  html += `</div>`;

  $('#page-content').innerHTML = html;

  $('#cron-install').onclick = () => cronAction('install');
  $('#cron-remove').onclick = () => cronAction('remove');
}

async function cronAction(action) {
  const isInstall = action === 'install';

  const ok = await confirmDialog({
    title: isInstall ? '安装定时任务' : '移除定时任务',
    text: isInstall
      ? '将安装备份、验证、健康检查三个定时任务。已有的 Nova 任务会被替换。'
      : '将移除面板管理的定时任务。你手动添加的其他任务不受影响。',
    okText: isInstall ? '安装' : '移除',
    danger: !isInstall,
  });
  if (!ok) return;

  try {
    const res = await api('POST', `/api/cron/${action}`);
    showModal(isInstall ? '安装结果' : '移除结果', res.ok, res.output, res.durationMs);
    await loadCron();
  } catch (e) {
    toast(e.message, 'err');
  }
}

// ============================================================ 日志

async function loadLogs() {
  const files = await api('GET', '/api/logs');
  state.data.logFiles = files;

  const current = state.data.logKey || (files.length ? files[0].key : '');
  state.data.logKey = current;

  let content = '';
  if (current) {
    try {
      const res = await api('GET', `/api/logs/${encodeURIComponent(current)}?lines=500`);
      content = res.content || '';
    } catch (e) {
      content = '读取失败：' + e.message;
    }
  }

  state.data.logContent = content;
  renderLogs();
}

function renderLogs() {
  const files = state.data.logFiles || [];
  const current = state.data.logKey || '';
  const content = state.data.logContent || '';

  let tabs = files.map((f) => {
    const active = f.key === current;
    return `<button class="btn btn-sm ${active ? 'btn-primary' : ''}"
              data-log="${esc(f.key)}">${esc(f.label)}</button>`;
  }).join('');

  let html = `
  <div class="card" style="margin-bottom:14px">
    <div class="card-head">
      <div>
        <h3>日志</h3>
        <div class="card-sub">显示最近 500 行，最新的在最上面</div>
      </div>
      <button class="btn btn-sm" id="log-refresh">刷新</button>
    </div>
    <div style="display:flex;gap:8px;flex-wrap:wrap">${tabs}</div>
  </div>

  <div class="card">
    <pre class="code-block" style="max-height:600px">${esc(content) || '（空）'}</pre>
  </div>`;

  $('#page-content').innerHTML = html;

  $('#page-content').querySelectorAll('[data-log]').forEach((btn) => {
    btn.onclick = () => {
      state.data.logKey = btn.dataset.log;
      loadLogs();
    };
  });

  $('#log-refresh').onclick = () => loadLogs();
}

// ============================================================ 审计

async function loadAudit() {
  const data = await api('GET', '/api/audit?page=1');
  state.data.audit = data;
  renderAudit();
}

function renderAudit() {
  const data = state.data.audit || { items: [], total: 0 };

  let html = `
  <div class="card">
    <div class="card-head">
      <div>
        <h3>操作记录</h3>
        <div class="card-sub">共 ${data.total} 条 · 所有写操作都会记录</div>
      </div>
    </div>`;

  if (!data.items.length) {
    html += `<div class="empty"><div class="big">✎</div><p>还没有操作记录</p></div>`;
  } else {
    html += `<table class="table">
      <thead><tr>
        <th>时间</th><th>操作人</th><th>操作</th><th>详情</th><th>IP</th><th>结果</th>
      </tr></thead>
      <tbody>`;

    for (const l of data.items) {
      html += `<tr>
        <td class="muted" style="font-size:12px;white-space:nowrap">${fmtTime(l.createdAt)}</td>
        <td>${esc(l.adminName)}</td>
        <td style="font-family:var(--mono);font-size:12px">${esc(l.action)}</td>
        <td class="muted" style="font-size:12px;max-width:280px;word-break:break-all">${esc(l.detail) || '—'}</td>
        <td class="muted" style="font-size:12px;font-family:var(--mono)">${esc(l.ip)}</td>
        <td>${l.ok ? '<span class="badge ok">成功</span>' : '<span class="badge err">失败</span>'}</td>
      </tr>`;
    }

    html += `</tbody></table>`;
  }

  html += `</div>`;

  $('#page-content').innerHTML = html;
}

// ============================================================ 设置

function renderSettings() {
  $('#page-content').innerHTML = `
  <div class="card" style="max-width:520px">
    <div class="card-head">
      <div>
        <h3>修改密码</h3>
        <div class="card-sub">修改后其他设备会立即下线</div>
      </div>
    </div>
    <label class="field">
      <span>当前密码</span>
      <input type="password" id="pwd-current" autocomplete="current-password">
    </label>
    <label class="field">
      <span>新密码（至少 8 位）</span>
      <input type="password" id="pwd-new" autocomplete="new-password">
    </label>
    <label class="field">
      <span>确认新密码</span>
      <input type="password" id="pwd-confirm" autocomplete="new-password">
    </label>
    <button class="btn btn-primary btn-block" id="pwd-save">修改密码</button>
  </div>

  <div class="card" style="max-width:520px;margin-top:14px">
    <div class="card-head"><h3>安全建议</h3></div>
    <div class="hint">
      · 面板默认只监听 127.0.0.1，通过 SSH 端口转发访问最安全<br>
      · 不要直接把面板端口开到公网<br>
      · 定期检查「操作记录」，发现异常登录立即改密码<br>
      · 备份 <code style="font-family:var(--mono);font-size:11.5px">/etc/nova/env</code>（含数据库密码）
    </div>
  </div>`;

  $('#pwd-save').onclick = async () => {
    const cur = $('#pwd-current').value;
    const nw = $('#pwd-new').value;
    const cf = $('#pwd-confirm').value;

    if (!cur || !nw) { toast('请填写完整', 'err'); return; }
    if (nw !== cf) { toast('两次输入的新密码不一致', 'err'); return; }
    if (nw.length < 8) { toast('新密码至少 8 位', 'err'); return; }

    try {
      const res = await api('POST', '/api/password', { current: cur, new: nw });
      toast(res.message || '密码已修改', 'ok');
      $('#pwd-current').value = '';
      $('#pwd-new').value = '';
      $('#pwd-confirm').value = '';
    } catch (e) {
      toast(e.message, 'err');
    }
  };
}

// ============================================================ 启动

(async function init() {
  try {
    const sess = await api('GET', '/api/session');

    if (sess.authenticated) {
      state.csrf = sess.csrfToken;
      state.user = sess.username;
      showApp();
      navigate('overview');
    } else {
      showLogin();
    }
  } catch (e) {
    showLogin();
  }
})();

// 快捷键：Ctrl+R 刷新当前页（不刷新整个浏览器）
document.addEventListener('keydown', (e) => {
  if ((e.ctrlKey || e.metaKey) && e.key === 'r') {
    e.preventDefault();
    if (!$('#app-view').classList.contains('hidden')) navigate(state.page);
  }
});
