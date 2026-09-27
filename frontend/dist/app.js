/* RapidProxy 界面逻辑
 * 与 Go 后端通过 Wails 注入的 window.go.main.App.* 通信。
 * 文案统一走 i18n.js 的 I18N.t()；语言偏好存于 Go 配置（设置页可切换）。 */

const S = {
  state: null,
  logs: [],
  login: { status: 'idle' },
  update: null,
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
/* 取当前语言文案的简写。 */
const t = (key, params) => I18N.t(key, params);

/* ------------------------------- 后端调用 ------------------------------- */

function backend() {
  return (window.go && window.go.main && window.go.main.App) || null;
}

async function call(method, ...args) {
  const api = backend();
  if (!api || typeof api[method] !== 'function') {
    throw new Error(t('toast.backendNotReady'));
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
    toast(t('toast.copied'));
  } catch (err) {
    toast(t('toast.copyFailedFmt', { err: String(err) }));
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

/* 每次渲染前同步语言偏好并刷新静态文案。
 * 语言偏好保存在 Go 配置里（state.settings.language），这里同步到 I18N；
 * localStorage 里还留有一份缓存供下次启动时提前生效（见 i18n.js）。 */
function syncLanguage() {
  const pref = (S.state && S.state.settings && S.state.settings.language) || 'system';
  if (pref !== I18N.pref) I18N.setPref(pref);
  I18N.apply();
}

function renderStatus() {
  const s = S.state;
  const pill = $('status-pill');
  pill.classList.toggle('on', !!s.running);
  pill.querySelector('em').textContent = s.running ? t('status.running') : t('status.stopped');
  $('status-addr').textContent = s.running ? s.openaiUrl : t('status.serviceDown');
  $('btn-toggle').textContent = s.running ? t('btn.stop') : t('btn.start');
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
    keyNode.textContent = t('key.none');
    delete keyNode.dataset.raw;
    $('btn-regen-key').textContent = t('key.gen');
    $('btn-eye').disabled = true;
  } else {
    keyNode.dataset.raw = keys[0];
    keyNode.textContent = S.revealKey ? keys[0] : maskKey(keys[0]);
    keyNode.classList.toggle('secret', !S.revealKey);
    $('btn-regen-key').textContent = t('key.regen');
    $('btn-eye').disabled = false;
  }
  $('btn-eye').textContent = S.revealKey ? t('btn.hideKey') : t('btn.show');
  $('info-require-key').textContent = keys.length
    ? t('ov.requireKey.yes')
    : t('ov.requireKey.no');
}

function renderOverview() {
  const s = S.state;
  $('ov-running').textContent = s.running ? t('status.running') : t('status.stopped');
  $('ov-listen').textContent = s.listen;
  $('ov-models').textContent = t('count.unit', { n: (s.models || []).length });
  const accCount = (s.accounts || []).length;
  $('ov-accounts').textContent = accCount > 0 ? t('count.unit', { n: accCount }) : t('ov.notLoggedIn');
  $('ov-sync').textContent = s.settings.modelSyncHours > 0 ? t('sync.every', { n: s.settings.modelSyncHours }) : t('sync.off');

  const list = $('ov-providers');
  list.innerHTML = '';
  (s.providers || []).forEach((p) => {
    const item = el('div', 'provider-item');
    const meta = el('div', 'meta');
    meta.appendChild(el('strong', null, p.name));
    meta.appendChild(el('span', null, p.baseUrl));
    item.appendChild(meta);
    const ops = el('div', 'ops');
    const tag = el('span', 'tag' + (p.enabled ? ' on' : ''), p.enabled ? t('tag.enabled') : t('tag.disabled'));
    ops.appendChild(tag);
    // 登录状态：有账号即视为已登录（绿色），否则醒目提示未登录
    const logged = p.accountCount > 0;
    ops.appendChild(el('span', 'tag' + (logged ? ' on' : ' warn'),
      logged ? t('tag.logged', { n: p.accountCount }) : t('tag.notLogged')));
    ops.appendChild(el('span', 'tag', t('tag.models', { n: p.modelCount })));
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
    login.status === 'pending' ? t('login.pending') :
    login.status === 'success' ? t('login.success') : t('login.failed');
  $('login-message').textContent = login.message || '';
  $('login-expires').textContent = login.status === 'pending' ? t('login.expiresFmt', { time: login.expiresAt }) : '';

  const urlNode = $('login-url');
  urlNode.textContent = login.url || '-';
  $('btn-open-login').disabled = login.status !== 'pending';
  $('btn-cancel-login').disabled = login.status !== 'pending';
}

function renderLoginProviders() {
  const wrap = $('login-providers');
  wrap.innerHTML = '';
  (S.state.providers || []).forEach((p) => {
    const btn = el('button', 'btn');
    btn.textContent = t('btn.loginFmt', { name: p.name });
    btn.onclick = () => startLogin(p.id);
    wrap.appendChild(btn);
    if (!p.enabled) {
      const note = el('span', 'hint', t('acc.loginDisabled'));
      wrap.appendChild(note);
    }
  });
}

function renderAccounts() {
  const list = $('account-list');
  list.innerHTML = '';
  const accounts = S.state.accounts || [];
  $('accounts-count').textContent = accounts.length ? t('acc.countFmt', { n: accounts.length }) : '';

  if (!accounts.length) {
    list.appendChild(el('div', 'empty', t('acc.empty')));
    return;
  }

  accounts.forEach((a) => {
    const item = el('div', 'account-item');
    const who = el('div', 'who');
    who.appendChild(el('strong', null, a.nickname || a.uid || a.id));
    who.appendChild(el('span', null, (a.providerName || a.provider) + ' · UID ' + (a.uid || t('acc.uidUnknown'))));
    who.appendChild(el('span', null, t('acc.signedAtFmt', { time: a.createdAt || '-' })));
    item.appendChild(who);

    const ops = el('div', 'ops');
    const badge = el('span', 'badge' + (a.expired ? ' warn' : ''), a.expiresIn || t('acc.expiresUnknown'));
    ops.appendChild(badge);
    const del = el('button', 'btn btn-mini btn-danger', t('btn.delete'));
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
    const cell = el('td', 'empty', t('models.noMatch'));
    cell.colSpan = 6;
    row.appendChild(cell);
    body.appendChild(row);
    return;
  }
  models.forEach((m) => {
    const row = el('tr');
    row.appendChild(el('td', 'mono', m.id));
    row.appendChild(el('td', null, m.name || '-'));
    const pcell = el('td');
    pcell.appendChild(el('span', 'tag on', m.provider || '-'));
    row.appendChild(pcell);
    row.appendChild(el('td', 'mono', formatTokens(m.context)));
    row.appendChild(el('td', 'mono', formatTokens(m.maxOut)));
    row.appendChild(el('td', null, m.images ? t('models.imgYes') : '—'));
    body.appendChild(row);
  });
}

function renderSettings() {
  const s = S.state;
  if (S.dirty) return; // 用户正在编辑时不要覆盖输入
  $('set-language').value = s.settings.language || 'system';
  $('set-listen').value = s.settings.listen;
  $('set-sync').value = s.settings.modelSyncHours;
  $('set-cors').checked = s.settings.cors;
  $('set-sanitize').checked = s.settings.sanitize;
  $('set-thinking').checked = s.settings.maxThinking;
  $('set-autostart').checked = s.settings.autoStart;
  $('set-launch').checked = s.settings.launchAtLogin !== false;

  const wrap = $('provider-config');
  wrap.innerHTML = '';
  (s.providers || []).forEach((p) => {
    const item = el('div', 'provider-item');
    const meta = el('div', 'meta');
    meta.appendChild(el('strong', null, p.name));
    meta.appendChild(el('span', null, t(p.enabled ? 'set.provider.enabledFmt' : 'set.provider.disabledFmt', { n: p.accountCount })));
    item.appendChild(meta);

    const ops = el('div', 'ops');
    const proxy = el('input', 'input');
    proxy.type = 'text';
    proxy.style.maxWidth = '230px';
    proxy.placeholder = p.needsProxy ? t('set.proxyNeeded') : t('set.proxyOptional');
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
    toggle.appendChild(el('span', null, t('set.enable')));
    ops.appendChild(toggle);
    item.appendChild(ops);
    wrap.appendChild(item);
  });

  const keyList = $('key-list');
  keyList.innerHTML = '';
  const keys = s.apiKeys || [];
  if (!keys.length) {
    keyList.appendChild(el('div', 'empty', t('set.key.empty')));
  } else {
    keys.forEach((key) => {
      const item = el('div', 'key-item');
      const code = el('code', null, key);
      item.appendChild(code);
      const copy = el('button', 'btn btn-mini', t('btn.copy'));
      copy.onclick = () => copyFrom(code);
      item.appendChild(copy);
      const del = el('button', 'btn btn-mini btn-danger', t('btn.delete'));
      del.onclick = () => removeKey(key);
      item.appendChild(del);
      keyList.appendChild(item);
    });
  }

  $('path-data').textContent = s.settings.dataDir;
  $('path-config').textContent = s.settings.configPath;
  $('path-accounts').textContent = s.settings.accountDir;
  $('path-log').textContent = s.settings.logPath || '-';
  $('path-version').textContent = t('ver.fmt', { v: s.settings.version, p: s.settings.platform });
  $('version').textContent = 'v' + s.settings.version;
  $('about-version').textContent = 'v' + s.settings.version;
  $('about-platform').textContent = s.settings.platform;
}

function renderAll() {
  if (!S.state) return;
  syncLanguage();
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
    toast(t('toast.stateFailedFmt', { err: String(err) }));
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
    toast(t('toast.loginStartFailedFmt', { err: String(err) }));
    await refresh();
  }
}

async function removeAccount(provider, id, name) {
  if (!confirm(t('confirm.deleteAccountFmt', { name: name }))) return;
  try {
    await call('DeleteAccount', provider, id);
    toast(t('toast.accountDeleted'));
    await refresh();
  } catch (err) {
    toast(t('toast.deleteFailedFmt', { err: String(err) }));
  }
}

async function removeKey(key) {
  try {
    await call('RemoveAPIKey', key);
    toast(t('toast.keyDeleted'));
    await refresh();
  } catch (err) {
    toast(t('toast.deleteFailedFmt', { err: String(err) }));
  }
}

async function toggleService() {
  $('btn-toggle').disabled = true;
  try {
    const running = await call('ToggleService');
    toast(running ? t('toast.serviceStarted') : t('toast.serviceStopped'));
    await refresh();
  } catch (err) {
    toast(t('toast.opFailedFmt', { err: String(err) }));
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
    launchAtLogin: $('set-launch').checked,
    profiles,
  };
  $('btn-save').disabled = true;
  $('save-hint').textContent = t('save.saving');
  try {
    await call('SaveSettings', input);
    S.dirty = false;
    $('save-hint').textContent = t('save.done');
    toast(t('save.done'));
    await refresh();
    setTimeout(() => { $('save-hint').textContent = ''; }, 2500);
  } catch (err) {
    $('save-hint').textContent = t('toast.opFailedFmt', { err: String(err) });
    toast(t('toast.opFailedFmt', { err: String(err) }));
  } finally {
    $('btn-save').disabled = false;
  }
}

/* 切换界面语言：保存到 Go 配置（托盘菜单同步切换），
 * 成功后立即应用翻译；Go 侧随后推送的新 state 会再次校准。 */
async function changeLanguage(value) {
  try {
    await call('SetLanguage', value);
    I18N.setPref(value);
    I18N.apply();
    if (S.state && S.state.settings) S.state.settings.language = value;
    $('set-language').value = value;
    toast(t('toast.langChanged'));
    await refresh();
  } catch (err) {
    toast(t('toast.opFailedFmt', { err: String(err) }));
    await refresh();
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
      toast(t('toast.windowReset'));
    } catch (err) {
      toast(t('toast.windowResetFailedFmt', { err: String(err) }));
    }
  };
  $('btn-quit').onclick = () => {
    if (confirm(t('confirm.quit'))) {
      call('QuitApp').catch(() => {});
    }
  };
  $('btn-eye').onclick = () => { S.revealKey = !S.revealKey; renderAccess(); };
  $('btn-regen-key').onclick = async () => {
    const keys = S.state.apiKeys || [];
    if (keys.length && !confirm(t('confirm.regenKey'))) return;
    try {
      if (keys.length) await call('RemoveAPIKey', keys[0]);
      const key = await call('GenerateAPIKey');
      S.revealKey = true;
      toast(t('toast.newKeyFmt', { key: maskKey(key) }));
      await refresh();
    } catch (err) {
      toast(t('toast.genFailedFmt', { err: String(err) }));
    }
  };
  $('btn-refresh').onclick = () => runSync();
  $('btn-sync-models').onclick = () => runSync();
  $('btn-open-dir').onclick = () => call('OpenDataDir').catch(() => {});
  $('btn-clear-log').onclick = () => call('ClearLogs').then(refresh).catch(() => {});
  $('btn-cancel-login').onclick = () => call('CancelLogin').then(refresh).catch(() => {});
  $('btn-open-login').onclick = () => {
    if (S.login.url) call('OpenURL', S.login.url).catch(() => {});
  };
  $('model-search').oninput = (e) => { S.modelFilter = e.target.value; renderModels(); };

  // 语言切换即时生效；其余设置项保持「编辑后点保存」的节奏
  $('set-language').onchange = (e) => changeLanguage(e.target.value);
  document.querySelectorAll('#tab-settings input, #tab-settings select').forEach((node) => {
    if (node.id === 'set-language') return;
    node.addEventListener('input', () => { S.dirty = true; });
    node.addEventListener('change', () => { S.dirty = true; });
  });
  $('btn-save').onclick = saveSettings;

  $('btn-add-key').onclick = async () => {
    const value = $('new-key').value.trim();
    if (!value) return toast(t('toast.keyEmpty'));
    try {
      await call('AddAPIKey', value);
      $('new-key').value = '';
      toast(t('toast.keyAdded'));
      await refresh();
    } catch (err) {
      toast(t('toast.addFailedFmt', { err: String(err) }));
    }
  };
  $('btn-gen-key').onclick = async () => {
    try {
      const key = await call('GenerateAPIKey');
      S.revealKey = true;
      toast(t('toast.keyGeneratedFmt', { key: maskKey(key) }));
      await refresh();
    } catch (err) {
      toast(t('toast.genFailedFmt', { err: String(err) }));
    }
  };

  // 在线更新
  $('btn-check-update').onclick = checkUpdate;
  $('btn-do-update').onclick = doUpdate;

  // 关于页：用系统浏览器打开 GitHub 仓库
  $('btn-open-repo').onclick = () => {
    try { window.runtime.BrowserOpenURL('https://github.com/RapidAI/RapidProxy'); }
    catch (err) { toast(t('toast.browserFailedFmt', { err: String(err) })); }
  };
}

async function runSync() {
  try {
    toast(t('toast.syncing'));
    await call('RefreshModels');
    toast(t('toast.synced'));
    await refresh();
  } catch (err) {
    toast(t('toast.syncFailedFmt', { err: String(err) }));
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
    if (payload.status === 'success') toast(t('login.success'));
    if (payload.status === 'failed') toast(t('login.failed') + (payload.message ? ': ' + payload.message : ''));
  });
  window.runtime.EventsOn('log', (line) => {
    S.logs.push(line);
    if (S.logs.length > 400) S.logs = S.logs.slice(-400);
    renderLogs();
  });
  window.runtime.EventsOn('update', (payload) => {
    S.update = payload;
    renderUpdate();
    if (payload.phase === 'available') toast(payload.message || t('toast.updateAvailable'));
    if (payload.phase === 'failed') toast(t('toast.updateFailedFmt', { err: payload.message || '' }));
  });
}

// ---------------- 在线更新 ----------------

function renderUpdate() {
  const u = S.update || {};
  const status = $('upd-status');
  const btnCheck = $('btn-check-update');
  const btnDo = $('btn-do-update');
  const bar = $('upd-progress');
  const fill = $('upd-bar');
  const msg = $('upd-message');
  if (!status) return;

  const cur = (S.state && S.state.settings && S.state.settings.version) || '';
  status.textContent = u.latest
    ? t('upd.status.fmt', { cur: cur, latest: u.latest })
    : t('upd.status.curOnly', { cur: cur });

  const busy = u.phase === 'checking' || u.phase === 'downloading' || u.phase === 'installing';
  btnCheck.disabled = busy;
  btnCheck.textContent = u.phase === 'checking' ? t('btn.checking') : t('btn.checkUpdate');
  btnDo.classList.toggle('hidden', u.phase !== 'available');

  bar.classList.toggle('hidden', u.phase !== 'downloading');
  if (u.phase === 'downloading') {
    fill.style.width = u.progress >= 0 ? u.progress + '%' : '40%';
    fill.classList.toggle('indeterminate', u.progress < 0);
  }

  msg.textContent = u.message || '';
}

async function checkUpdate() {
  S.update = { phase: 'checking' };
  renderUpdate();
  try {
    S.update = await call('CheckUpdate');
  } catch (err) {
    S.update = { phase: 'failed', message: String(err) };
  }
  renderUpdate();
}

async function doUpdate() {
  try {
    await call('DoUpdate');
  } catch (err) {
    S.update = { phase: 'failed', message: String(err) };
    renderUpdate();
  }
}

async function main() {
  // 后端就绪前先按缓存的偏好应用语言，避免启动闪变
  I18N.apply();
  await waitForBackend();
  bindUI();
  bindEvents();
  await refresh();
  setInterval(refresh, 20000);
}

window.addEventListener('DOMContentLoaded', main);
