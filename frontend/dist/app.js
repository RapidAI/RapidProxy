/* RapidProxy 界面逻辑
 * 与 Go 后端通过 Wails 注入的 window.go.main.App.* 通信。 */

const S = {
  state: null,
  logs: [],
  login: { status: 'idle' },
  revealKey: false,
  modelFilter: '',
  dirty: false,
};

const $ = (id) => document.getElementById(id);
const el = (tag, cls, text) => {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  if (text !== undefined) node.textContent = text;
  return node;
};

/* ------------------------------- 后端调用 ------------------------------- */

function backend() {
  return (window.go && window.go.main && window.go.main.App) || null;
}

async function call(method, ...args) {
  const api = backend();
  if (!api || typeof api[method] !== 'function') {
    throw new Error('后端尚未就绪，请稍后重试');
  }
  return api[method](...args);
}

function waitForBackend() {
  return new Promise((resolve) => {
    const tick = () => {
      if (backend()) return resolve();
      setTimeout(tick, 60);
    };
    tick();
  });
}

function toast(message) {
  const node = $('toast');
  node.textContent = message;
  node.classList.add('show');
  clearTimeout(toast._timer);
  toast._timer = setTimeout(() => node.classList.remove('show'), 1800);
}

async function copyFrom(node) {
  const value = node.dataset.raw || node.textContent;
  if (!value || value === '-') return;
  try {
    await call('CopyText', value);
    toast('已复制到剪贴板');
  } catch (err) {
    toast('复制失败：' + err);
  }
}

/* ------------------------------- 格式化 ------------------------------- */

function formatTokens(value) {
  if (!value) return '-';
  if (value >= 1000000) return (value / 1000000).toFixed(value % 1000000 === 0 ? 0 : 1) + 'M';
  if (value >= 1000) return Math.round(value / 1000) + 'K';
  return String(value);
}

function maskKey(key) {
  if (!key) return '-';
  if (key.length <= 12) return key.slice(0, 3) + '••••';
  return key.slice(0, 10) + '••••' + key.slice(-4);
}

/* ------------------------------- 渲染 ------------------------------- */

function renderStatus() {
  const s = S.state;
  const pill = $('status-pill');
  pill.classList.toggle('on', !!s.running);
  pill.querySelector('em').textContent = s.running ? '运行中' : '已停止';
  $('status-addr').textContent = s.running ? s.openaiUrl : '服务未启动';
  $('btn-toggle').textContent = s.running ? '停止服务' : '启动服务';
}

function renderAccess() {
  const s = S.state;
  const openaiUrl = s.openaiUrl;
  const modelsUrl = openaiUrl + '/models';

  const urlNode = $('info-openai-url');
  urlNode.textContent = openaiUrl;
  delete urlNode.dataset.raw;

  const modelsNode = $('info-models-url');
  modelsNode.textContent = modelsUrl;
  delete modelsNode.dataset.raw;

  const keyNode = $('info-api-key');
  const keys = s.apiKeys || [];
  if (keys.length === 0) {
    keyNode.textContent = '未设置（任何密钥均可访问）';
    delete keyNode.dataset.raw;
    $('btn-regen-key').textContent = '生成密钥';
    $('btn-eye').disabled = true;
  } else {
    keyNode.dataset.raw = keys[0];
    keyNode.textContent = S.revealKey ? keys[0] : maskKey(keys[0]);
    keyNode.classList.toggle('secret', !S.revealKey);
    $('btn-regen-key').textContent = '重新生成';
    $('btn-eye').disabled = false;
  }
  $('btn-eye').textContent = S.revealKey ? '隐藏' : '显示';
  $('info-require-key').textContent = keys.length
    ? '客户端需要在请求头携带该密钥'
    : '当前不校验密钥，便于本地调试';
}

function renderOverview() {
  const s = S.state;
  $('ov-running').textContent = s.running ? '运行中' : '已停止';
  $('ov-listen').textContent = s.listen;
  $('ov-models').textContent = (s.models || []).length + ' 个';
  $('ov-accounts').textContent = (s.accounts || []).length + ' 个';
  $('ov-sync').textContent = s.settings.modelSyncHours > 0 ? '每 ' + s.settings.modelSyncHours + ' 小时' : '已关闭';

  const list = $('ov-providers');
  list.innerHTML = '';
  (s.providers || []).forEach((p) => {
    const item = el('div', 'provider-item');
    const meta = el('div', 'meta');
    meta.appendChild(el('strong', null, p.name));
    meta.appendChild(el('span', null, p.baseUrl));
    item.appendChild(meta);
    const ops = el('div', 'ops');
    const tag = el('span', 'tag' + (p.enabled ? ' on' : ''), p.enabled ? '已启用' : '未启用');
    ops.appendChild(tag);
    ops.appendChild(el('span', 'tag', p.accountCount + ' 账号'));
    ops.appendChild(el('span', 'tag', p.modelCount + ' 模型'));
    item.appendChild(ops);
    list.appendChild(item);
  });
}

function renderLogs() {
  const view = $('log-view');
  view.textContent = S.logs.join('\n');
  view.scrollTop = view.scrollHeight;
}

function renderLogin() {
  const login = S.login;
  const box = $('login-box');
  const active = login.status === 'pending' || login.status === 'success' || login.status === 'failed';
  box.classList.toggle('hidden', !active);
  if (!active) return;

  const spinner = $('login-spinner');
  spinner.className = 'spinner' + (login.status === 'success' ? ' done' : login.status === 'failed' ? ' fail' : '');

  $('login-title').textContent =
    login.status === 'pending' ? '等待完成登录' :
    login.status === 'success' ? '登录成功' : '登录失败';
  $('login-message').textContent = login.message || '';
  $('login-expires').textContent = login.status === 'pending' ? '链接有效期至 ' + login.expiresAt : '';

  // 应用内授权窗口：仅 pending 时展示，成功/失败后收起
  const wrap = $('login-frame-wrap');
  const frame = $('login-frame');
  if (login.status === 'pending' && login.embedUrl) {
    if (!frame.src || frame.src !== login.embedUrl) {
      frame.src = login.embedUrl;
    }
    wrap.classList.remove('hidden');
  } else {
    wrap.classList.add('hidden');
    if (frame.src) frame.removeAttribute('src');
  }

  const urlNode = $('login-url');
  urlNode.textContent = login.url || '-';
  $('btn-open-login').disabled = login.status !== 'pending';
  $('btn-cancel-login').disabled = login.status !== 'pending';
}

// 官方登录页在应用内嵌入（embed=iframe）模式下，登录结果会 postMessage 给父页面。
// token 本身由后端轮询获取，这里只负责及时感知结果、给出提示。
function setupLoginMessageBridge() {
  window.addEventListener('message', (ev) => {
    const login = S.login;
    if (!login || login.status !== 'pending' || !login.url) return;
    let expected;
    try { expected = new URL(login.url).origin; } catch { return; }
    if (!expected || ev.origin !== expected) return;
    const d = ev.data;
    if (!d || typeof d !== 'object') return;
    if (d.type === 'login_success') {
      toast('授权成功，正在保存登录凭据…');
    } else if (d.type === 'login_fail') {
      const reason = d.errorDescription || d.error || '未知错误';
      toast('授权失败：' + reason);
    }
  });
}

function renderLoginProviders() {
  const wrap = $('login-providers');
  wrap.innerHTML = '';
  (S.state.providers || []).forEach((p) => {
    const btn = el('button', 'btn');
    btn.textContent = '登录 ' + p.name;
    btn.onclick = () => startLogin(p.id);
    wrap.appendChild(btn);
    if (!p.enabled) {
      const note = el('span', 'hint', '（未启用，登录后可到设置里启用）');
      wrap.appendChild(note);
    }
  });
}

function renderAccounts() {
  const list = $('account-list');
  list.innerHTML = '';
  const accounts = S.state.accounts || [];
  $('accounts-count').textContent = accounts.length ? accounts.length + ' 个账号' : '';

  if (!accounts.length) {
    list.appendChild(el('div', 'empty', '还没有登录任何账号，点击上方按钮开始登录'));
    return;
  }

  accounts.forEach((a) => {
    const item = el('div', 'account-item');
    const who = el('div', 'who');
    who.appendChild(el('strong', null, a.nickname || a.uid || a.id));
    who.appendChild(el('span', null, (a.providerName || a.provider) + ' · UID ' + (a.uid || '未知')));
    who.appendChild(el('span', null, '登录于 ' + (a.createdAt || '-')));
    item.appendChild(who);

    const ops = el('div', 'ops');
    const badge = el('span', 'badge' + (a.expired ? ' warn' : ''), a.expiresIn || '有效期未知');
    ops.appendChild(badge);
    const del = el('button', 'btn btn-mini btn-danger', '删除');
    del.onclick = () => removeAccount(a.provider, a.id, a.nickname || a.id);
    ops.appendChild(del);
    item.appendChild(ops);
    list.appendChild(item);
  });
}

function renderModels() {
  const body = $('model-body');
  body.innerHTML = '';
  const filter = S.modelFilter.trim().toLowerCase();
  const models = (S.state.models || []).filter((m) => {
    if (!filter) return true;
    return (m.id + ' ' + (m.name || '') + ' ' + (m.provider || '')).toLowerCase().includes(filter);
  });
  if (!models.length) {
    const row = el('tr');
    const cell = el('td', 'empty', '没有匹配的模型');
    cell.colSpan = 6;
    row.appendChild(cell);
    body.appendChild(row);
    return;
  }
  models.forEach((m) => {
    const row = el('tr');
    row.appendChild(el('td', 'mono', m.id));
    row.appendChild(el('td', null, m.name || '-'));
    row.appendChild(el('td', null, m.provider || '-'));
    row.appendChild(el('td', 'mono', formatTokens(m.context)));
    row.appendChild(el('td', 'mono', formatTokens(m.maxOut)));
    row.appendChild(el('td', null, m.images ? '支持' : '—'));
    body.appendChild(row);
  });
}

function renderSettings() {
  const s = S.state;
  if (S.dirty) return; // 用户正在编辑时不要覆盖输入
  $('set-listen').value = s.settings.listen;
  $('set-sync').value = s.settings.modelSyncHours;
  $('set-cors').checked = s.settings.cors;
  $('set-sanitize').checked = s.settings.sanitize;
  $('set-thinking').checked = s.settings.maxThinking;
  $('set-autostart').checked = s.settings.autoStart;

  const wrap = $('provider-config');
  wrap.innerHTML = '';
  (s.providers || []).forEach((p) => {
    const item = el('div', 'provider-item');
    const meta = el('div', 'meta');
    meta.appendChild(el('strong', null, p.name));
    meta.appendChild(el('span', null, (p.enabled ? '已启用 · ' : '未启用 · ') + p.accountCount + ' 个账号'));
    item.appendChild(meta);

    const ops = el('div', 'ops');
    const proxy = el('input', 'input');
    proxy.type = 'text';
    proxy.style.maxWidth = '230px';
    proxy.placeholder = p.needsProxy ? '代理，如 http://127.0.0.1:7890' : '代理（留空跟随系统）';
    proxy.value = p.proxy || '';
    proxy.dataset.provider = p.id;
    proxy.oninput = () => { S.dirty = true; };
    ops.appendChild(proxy);

    const toggle = el('label', 'switch');
    const check = el('input');
    check.type = 'checkbox';
    check.checked = p.enabled;
    check.dataset.provider = p.id;
    check.onchange = () => { S.dirty = true; };
    toggle.appendChild(check);
    toggle.appendChild(el('span', null, '启用'));
    ops.appendChild(toggle);
    item.appendChild(ops);
    wrap.appendChild(item);
  });

  const keyList = $('key-list');
  keyList.innerHTML = '';
  const keys = s.apiKeys || [];
  if (!keys.length) {
    keyList.appendChild(el('div', 'empty', '尚未设置 API Key，任何人都可以访问本代理'));
  } else {
    keys.forEach((key) => {
      const item = el('div', 'key-item');
      const code = el('code', null, key);
      item.appendChild(code);
      const copy = el('button', 'btn btn-mini', '复制');
      copy.onclick = () => copyFrom(code);
      item.appendChild(copy);
      const del = el('button', 'btn btn-mini btn-danger', '删除');
      del.onclick = () => removeKey(key);
      item.appendChild(del);
      keyList.appendChild(item);
    });
  }

  $('path-data').textContent = s.settings.dataDir;
  $('path-config').textContent = s.settings.configPath;
  $('path-accounts').textContent = s.settings.accountDir;
  $('path-log').textContent = s.settings.logPath || '-';
  $('path-version').textContent = 'v' + s.settings.version + '（' + s.settings.platform + '）';
  $('version').textContent = 'v' + s.settings.version;
}

function renderAll() {
  if (!S.state) return;
  renderStatus();
  renderAccess();
  renderOverview();
  renderLogs();
  renderLogin();
  renderLoginProviders();
  renderAccounts();
  renderModels();
  renderSettings();
  refreshSample();
}

/* ------------------------------- 动作 ------------------------------- */

async function refreshSample() {
  try {
    const info = await call('GetAccessInfo');
    const node = $('info-sample');
    node.textContent = info.sample;
    node.dataset.raw = info.sample;
  } catch (err) {
    /* 忽略 */
  }
}

async function refresh() {
  try {
    S.state = await call('GetState');
    S.logs = S.state.logs || [];
    S.login = S.state.login || { status: 'idle' };
    renderAll();
  } catch (err) {
    toast('读取状态失败：' + err);
  }
}

async function startLogin(provider) {
  try {
    // 传自己的 origin，Go 侧据此构造应用内 iframe 授权链接（官方 embed=iframe 协议）
    const origin = (window.location && window.location.origin) || '';
    const view = await call('StartLogin', provider, origin);
    S.login = view;
    renderLogin();
  } catch (err) {
    toast('发起登录失败：' + err);
    await refresh();
  }
}

async function removeAccount(provider, id, name) {
  if (!confirm('确定删除账号「' + name + '」吗？')) return;
  try {
    await call('DeleteAccount', provider, id);
    toast('已删除账号');
    await refresh();
  } catch (err) {
    toast('删除失败：' + err);
  }
}

async function removeKey(key) {
  try {
    await call('RemoveAPIKey', key);
    toast('已删除密钥');
    await refresh();
  } catch (err) {
    toast('删除失败：' + err);
  }
}

async function toggleService() {
  $('btn-toggle').disabled = true;
  try {
    const running = await call('ToggleService');
    toast(running ? '服务已启动' : '服务已停止');
    await refresh();
  } catch (err) {
    toast('操作失败：' + err);
  } finally {
    $('btn-toggle').disabled = false;
  }
}

async function saveSettings() {
  const profiles = Array.from(document.querySelectorAll('#provider-config input[type=checkbox]')).map((c) => ({
    id: c.dataset.provider,
    enabled: c.checked,
    proxy: (document.querySelector('input[data-provider="' + c.dataset.provider + '"][type=text]') || {}).value || '',
  }));
  const input = {
    listen: $('set-listen').value.trim(),
    modelSyncHours: parseInt($('set-sync').value, 10) || 0,
    cors: $('set-cors').checked,
    sanitize: $('set-sanitize').checked,
    maxThinking: $('set-thinking').checked,
    autoStart: $('set-autostart').checked,
    profiles,
  };
  $('btn-save').disabled = true;
  $('save-hint').textContent = '正在保存…';
  try {
    await call('SaveSettings', input);
    S.dirty = false;
    $('save-hint').textContent = '已保存';
    toast('设置已保存');
    await refresh();
    setTimeout(() => { $('save-hint').textContent = ''; }, 2500);
  } catch (err) {
    $('save-hint').textContent = '保存失败：' + err;
    toast('保存失败：' + err);
  } finally {
    $('btn-save').disabled = false;
  }
}

/* ------------------------------- 事件绑定 ------------------------------- */

function bindUI() {
  document.querySelectorAll('.nav-item').forEach((btn) => {
    btn.onclick = () => {
      document.querySelectorAll('.nav-item').forEach((n) => n.classList.remove('active'));
      document.querySelectorAll('.tab').forEach((t) => t.classList.remove('active'));
      btn.classList.add('active');
      $('tab-' + btn.dataset.tab).classList.add('active');
    };
  });

  document.querySelectorAll('[data-copy]').forEach((btn) => {
    btn.onclick = () => copyFrom($(btn.dataset.copy));
  });

  $('btn-toggle').onclick = toggleService;
  $('btn-hide').onclick = () => call('HideWindow').catch(() => {});
  $('btn-reset-window').onclick = async () => {
    try {
      await call('ResetWindow');
      toast('窗口已按当前屏幕重新居中');
    } catch (err) {
      toast('重置窗口失败：' + err);
    }
  };
  $('btn-quit').onclick = () => {
    if (confirm('确定退出 RapidProxy 吗？退出后代理服务将停止。')) {
      call('QuitApp').catch(() => {});
    }
  };
  $('btn-eye').onclick = () => { S.revealKey = !S.revealKey; renderAccess(); };
  $('btn-regen-key').onclick = async () => {
    const keys = S.state.apiKeys || [];
    if (keys.length && !confirm('重新生成会替换当前密钥，已配置的软件需要同步更新。继续吗？')) return;
    try {
      if (keys.length) await call('RemoveAPIKey', keys[0]);
      const key = await call('GenerateAPIKey');
      S.revealKey = true;
      toast('新密钥已生成：' + maskKey(key));
      await refresh();
    } catch (err) {
      toast('生成失败：' + err);
    }
  };
  $('btn-refresh').onclick = () => runSync('模型已同步');
  $('btn-sync-models').onclick = () => runSync('模型已同步');
  $('btn-open-dir').onclick = () => call('OpenDataDir').catch(() => {});
  $('btn-clear-log').onclick = () => call('ClearLogs').then(refresh).catch(() => {});
  $('btn-cancel-login').onclick = () => call('CancelLogin').then(refresh).catch(() => {});
  $('btn-open-login').onclick = () => {
    if (S.login.url) call('OpenURL', S.login.url).catch(() => {});
  };
  $('model-search').oninput = (e) => { S.modelFilter = e.target.value; renderModels(); };

  document.querySelectorAll('#tab-settings input, #tab-settings select').forEach((node) => {
    node.addEventListener('input', () => { S.dirty = true; });
    node.addEventListener('change', () => { S.dirty = true; });
  });
  $('btn-save').onclick = saveSettings;

  $('btn-add-key').onclick = async () => {
    const value = $('new-key').value.trim();
    if (!value) return toast('请先填写密钥');
    try {
      await call('AddAPIKey', value);
      $('new-key').value = '';
      toast('已添加密钥');
      await refresh();
    } catch (err) {
      toast('添加失败：' + err);
    }
  };
  $('btn-gen-key').onclick = async () => {
    try {
      const key = await call('GenerateAPIKey');
      S.revealKey = true;
      toast('已生成：' + maskKey(key));
      await refresh();
    } catch (err) {
      toast('生成失败：' + err);
    }
  };
}

async function runSync(message) {
  try {
    toast('正在同步…');
    await call('RefreshModels');
    toast(message);
    await refresh();
  } catch (err) {
    toast('同步失败：' + err);
  }
}

function bindEvents() {
  if (!window.runtime) return;
  window.runtime.EventsOn('state', (payload) => {
    S.state = payload;
    S.login = payload.login || { status: 'idle' };
    renderAll();
  });
  window.runtime.EventsOn('login', (payload) => {
    S.login = payload;
    renderLogin();
    if (payload.status === 'success') toast('登录成功');
    if (payload.status === 'failed') toast('登录失败：' + (payload.message || ''));
  });
  window.runtime.EventsOn('log', (line) => {
    S.logs.push(line);
    if (S.logs.length > 400) S.logs = S.logs.slice(-400);
    renderLogs();
  });
}

async function main() {
  await waitForBackend();
  bindUI();
  bindEvents();
  setupLoginMessageBridge();
  await refresh();
  setInterval(refresh, 20000);
}

window.addEventListener('DOMContentLoaded', main);
