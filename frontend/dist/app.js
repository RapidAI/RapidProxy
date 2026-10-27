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
  modelProvider: '', // 模型页的上游筛选：'' 表示全部上游，否则是 providerId
  dirty: false,
  stats: { period: 'day', provider: '', model: '' },
  // 虚拟模型编辑器：editing 是正在编辑的原组名（新建时为空串），
  // members 是编辑器里那份可增删改的成员副本（保存前不落库）。
  vm: { editing: '', members: [] },
};

// OpenCode 密钥提交进行中：后端会联机校验，期间要挡住轮询刷新重复放开按钮。
let submittingKey = false;

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

/* 把秒数写成短文本：45s / 5m / 1h20m。 */
function formatDuration(seconds) {
  const s = Math.max(0, Math.round(Number(seconds) || 0));
  if (s < 60) return s + 's';
  const m = Math.floor(s / 60);
  if (m < 60) return m + 'm';
  const h = Math.floor(m / 60);
  const rest = m % 60;
  return rest ? h + 'h' + rest + 'm' : h + 'h';
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

  // 登录有三种输入形态：
  //   needCode —— OAuth 但回调页由上游托管（Claude），用户把页面上的授权码
  //               粘贴回来；
  //   needKey  —— API Key 制（OpenCode / 用户自建组），粘贴密钥；
  //   其余      —— 浏览器授权，本地回调或后台轮询自动收口。
  // 本机导入（state === 'local'）是 OpenCode 分支的变体：不自动登录，等用户
  // 确认（空提交 = 使用本机密钥）或粘贴新密钥覆盖。
  const pending = login.status === 'pending';
  const needCode = !!login.needCode && pending;
  const needKey = !!login.needKey && pending && !needCode;
  const wantsInput = needCode || needKey;
  const localImport = needKey && login.state === 'local';
  const providerName = loginProviderName(login.provider);
  const openCode = login.provider === 'opencode';
  const pasteTitle = openCode
    ? t('login.pasteTitle')
    : t('login.pasteTitle.custom', { name: providerName });
  $('login-title').textContent =
    login.status === 'success' ? t('login.success') :
    login.status === 'failed' ? t('login.failed') :
    localImport ? t('login.localTitle') :
    needCode ? t('login.codeTitle', { name: providerName }) :
    needKey ? pasteTitle : t('login.pending');
  $('login-message').textContent = login.message || '';
  $('login-expires').textContent = pending ? t('login.expiresFmt', { time: login.expiresAt }) : '';
  $('login-browser-tip').classList.toggle('hidden', wantsInput);
  $('login-key-tip').classList.toggle('hidden', !wantsInput);
  $('login-key-tip-text').textContent = needCode
    ? t('login.codeTip', { name: providerName })
    : localImport ? t('login.localTip')
    : openCode ? t('login.keyTip') : t('login.keyTip.custom', { name: providerName });
  $('login-key-row').classList.toggle('hidden', !wantsInput);
  $('login-key').placeholder = needCode
    ? t('login.codePlaceholder')
    : localImport ? t('login.localPlaceholder')
    : openCode ? t('login.keyPlaceholder') : t('login.keyPlaceholder.custom', { name: providerName });
  $('btn-submit-key').textContent = needCode
    ? t('btn.submitCode')
    : localImport ? t('btn.confirmLogin') : t('btn.submitKey');
  // 提交过程中后端要联机校验密钥，轮询刷新不能把按钮又放开导致重复提交。
  $('btn-submit-key').disabled = !wantsInput || submittingKey;
  if (!wantsInput && !submittingKey) $('login-key').value = '';

  // 没有授权链接时（粘贴密钥 / 粘贴授权码）整行隐藏，别让用户对着一个只有
  // 「-」和「复制链接」的空行发呆；「重新打开登录页」改用服务商控制台地址。
  const urlNode = $('login-url');
  urlNode.textContent = login.url || '-';
  $('login-url-row').classList.toggle('hidden', !login.url);
  const consoleURL = login.url ? '' : providerConsoleURL(login.provider);
  $('btn-open-login').classList.toggle('hidden', !login.url && !consoleURL);
  $('btn-open-login').textContent = login.url ? t('btn.reopenLogin') : t('btn.openConsole');
  $('btn-open-login').disabled = !pending;
  $('btn-cancel-login').disabled = !pending;
}

/* 按上游 ID 取显示名（登录面板的提示文案要用它）。 */
function loginProviderName(id) {
  const p = (S.state.providers || []).find((x) => x.id === id);
  return (p && p.name) || id || '';
}

/* custom 上游（API Key 制）没有授权链接，退而求其次给出服务商控制台地址。 */
function providerConsoleURL(id) {
  const p = (S.state.providers || []).find((x) => x.id === id);
  return (p && p.origin) || '';
}

function renderLoginProviders() {
  const wrap = $('login-providers');
  wrap.innerHTML = '';
  (S.state.providers || []).forEach((p) => {
    const btn = el('button', 'btn login-btn');
    btn.appendChild(el('span', null, t('btn.loginFmt', { name: p.name })));
    // 登录状态只用按钮末尾的小圆点表示：绿色 = 已登录，灰色 = 未登录。
    // 未启用的说明移进圆点的 title，免得每个按钮后面都拖一行文字。
    const logged = p.accountCount > 0;
    const dot = el('i', 'login-dot' + (logged ? ' on' : ''));
    dot.title = logged ? t('tag.logged', { n: p.accountCount }) : t('tag.notLogged');
    if (!p.enabled) dot.title += ' ' + t('acc.loginDisabled');
    btn.title = dot.title;
    btn.appendChild(dot);
    btn.onclick = () => startLogin(p.id);
    wrap.appendChild(btn);
  });
}

/* 重建列表会把正在编辑的输入框一起丢掉——20 秒一次的轮询刷新就会触发，
 * 用户刚敲进去的权重会被清空。这里记下焦点所在的输入框和光标位置，
 * 重建后再放回去。 */
function keepFocus(list, build) {
  const ae = document.activeElement;
  const key = ae && ae.dataset ? ae.dataset.focusKey : '';
  const value = key && 'value' in ae ? ae.value : null;
  const pos = key && 'selectionStart' in ae ? ae.selectionStart : null;
  build();
  if (!key) return;
  const next = list.querySelector('[data-focus-key="' + CSS.escape(key) + '"]');
  if (!next) return;
  // 用户已经改过但还没提交的内容不能被后端的值覆盖回去。
  if (value !== null && next.value !== value) next.value = value;
  next.focus();
  if (pos !== null && next.setSelectionRange) {
    try { next.setSelectionRange(pos, pos); } catch (e) { /* 部分输入类型不支持 */ }
  }
}

/* 账号按「版本」（上游）分组：只有同一组内的账号才会互相负载均衡，
 * 国内版与国际版的同名模型互不干扰。 */
function renderAccounts() {
  const list = $('account-list');
  const accounts = S.state.accounts || [];
  $('accounts-count').textContent = accounts.length ? t('acc.countFmt', { n: accounts.length }) : '';

  keepFocus(list, () => {
    list.innerHTML = '';

    if (!accounts.length) {
      list.appendChild(el('div', 'empty', t('acc.empty')));
      return;
    }

    // 分组顺序跟随上游配置，保证国内版 / 国际版的展示顺序稳定。
    const order = (S.state.providers || []).map((p) => p.id);
    const groups = new Map();
    accounts.forEach((a) => {
      if (!groups.has(a.provider)) groups.set(a.provider, []);
      groups.get(a.provider).push(a);
    });
    const keys = order.filter((id) => groups.has(id));
    groups.forEach((_, id) => {
      if (!keys.includes(id)) keys.push(id);
    });

    keys.forEach((pid) => {
      const members = groups.get(pid);
      const group = el('div', 'account-group');
      const head = el('div', 'group-head');
      const name = members[0].providerName || members[0].provider || pid;
      head.appendChild(el('strong', null, name));
      head.appendChild(el('span', 'hint', t('acc.group.countFmt', { n: members.length })));
      const allPaused = members.every((a) => a.paused);
      head.appendChild(el('span', allPaused ? 'badge warn' : 'hint', allPaused ? t('acc.pausedNote') : t('acc.group.hint')));
      group.appendChild(head);

      members.forEach((a) => group.appendChild(accountRow(a)));
      list.appendChild(group);
    });
  });
}

/* 单个账号行：权重输入 + 暂停/恢复 + 删除。 */
function accountRow(a) {
  const item = el('div', 'account-item' + (a.paused ? ' paused' : ''));
  const who = el('div', 'who');
  who.appendChild(el('strong', null, (a.nickname || a.uid || a.id) + (a.paused ? ' · ' + t('acc.paused') : '')));
  who.appendChild(el('span', null, 'UID ' + (a.uid || t('acc.uidUnknown'))));
  who.appendChild(el('span', null, t('acc.signedAtFmt', { time: a.createdAt || '-' })));
  if (a.cooldown > 0) {
    const reason = a.cooldownReason === 'credit'
      ? t('acc.cooldownCredit')
      : (a.cooldownReason === 'server' ? t('acc.cooldownServer') : t('acc.cooldownRate'));
    who.appendChild(el('span', 'cooldown-note', t('acc.cooldownFmt', { reason: reason, time: formatDuration(a.cooldown) })));
  }
  item.appendChild(who);

  const ops = el('div', 'ops');

  const weightBox = el('label', 'weight-box');
  weightBox.appendChild(el('span', null, t('acc.weight')));
  const weight = el('input', 'input weight-input');
  weight.type = 'number';
  weight.min = '0';
  weight.max = '1000';
  weight.step = '1';
  weight.value = String(a.weight || 1);
  weight.title = t('acc.weight.hint');
  weight.dataset.focusKey = 'w:' + a.provider + ':' + a.id;
  weight.onchange = () => setWeight(a, weight.value);
  weightBox.appendChild(weight);
  ops.appendChild(weightBox);

  const toggle = el('button', 'btn btn-mini', a.paused ? t('btn.resume') : t('btn.pause'));
  toggle.onclick = () => setPaused(a, !a.paused);
  ops.appendChild(toggle);

  const badge = el('span', 'badge' + (a.expired ? ' warn' : ''), a.expiresIn || t('acc.expiresUnknown'));
  ops.appendChild(badge);

  const del = el('button', 'btn btn-mini btn-danger', t('btn.delete'));
  del.onclick = () => removeAccount(a.provider, a.id, a.nickname || a.id);
  ops.appendChild(del);

  item.appendChild(ops);
  return item;
}

function setWeight(a, raw) {
  const value = parseInt(raw, 10);
  if (!Number.isFinite(value) || value < 0 || value > 1000) {
    toast(t('toast.weightBad'));
    return;
  }
  guard(a, () => call('SetAccountWeight', a.provider, a.id, value), t('toast.weightSaved'));
}

function setPaused(a, paused) {
  guard(a, () => call('SetAccountPaused', a.provider, a.id, paused), paused ? t('toast.paused') : t('toast.resumed'));
}

/* 模型归属的上游 ID：后端给的是 providerId，缺值时退回显示名，避免有行过滤不到。 */
const modelProviderID = (m) => m.providerId || m.provider || '';

/* 把模型按上游归堆，保持原顺序（与配置里的上游顺序一致），并带上每个上游的模型数。 */
function providerBuckets(models) {
  const buckets = [];
  const index = new Map();
  models.forEach((m) => {
    const id = modelProviderID(m);
    const hit = index.get(id);
    if (hit) {
      hit.count += 1;
      return;
    }
    const bucket = { id: id, name: m.provider || id || '-', count: 1 };
    index.set(id, bucket);
    buckets.push(bucket);
  });
  return buckets;
}

/* 上次渲染模型页的签名。
 *
 * 界面每 20 秒会被轮询重渲染一次：数据和文案都没变时必须跳过重建，否则
 * 用户刚在单元格里选中的文字会被抹掉——右键菜单里就有「复制选中内容」，
 * 选中后还没右键就被刷新清掉等于这个功能半残。
 *
 * 这里不能用账号列表那套 keepFocus：它恢复的是输入框的焦点与光标，
 * 而普通单元格里的文本选区没有等价物，只能不去动它。判定方式与流量图的
 * _statsSig 一致，顺便也省掉了每 20 秒一次的无谓重排。 */
let _modelFilterSig = '';
let _modelTableSig = '';

function renderModelFilters(buckets, total) {
  const row = $('model-providers');
  // 列表本身为空时没有可筛的维度，整行藏起来，避免留一排 0 计数。
  $('model-filter-row').classList.toggle('hidden', !total);
  const sig = [I18N.lang, total, buckets.map((b) => b.id + ':' + b.name + ':' + b.count).join(',')].join('|');
  if (sig !== _modelFilterSig) {
    _modelFilterSig = sig;
    row.innerHTML = '';
    const chip = (label, count, providerID) => {
      const node = el('button', 'chip', label);
      node.type = 'button';
      node.dataset.pid = providerID;
      node.appendChild(el('span', 'count', String(count)));
      node.onclick = () => {
        S.modelProvider = providerID;
        renderModels();
      };
      row.appendChild(node);
    };
    chip(t('models.allUpstream'), total, '');
    buckets.forEach((b) => chip(b.name, b.count, b.id));
  }
  // 选中态与计数文案跟标签集合无关，无论是否重建都要同步一次。
  Array.from(row.children).forEach((node) => {
    node.classList.toggle('active', (node.dataset.pid || '') === S.modelProvider);
  });
  const shown = S.modelProvider ? (buckets.find((b) => b.id === S.modelProvider) || {}).count || 0 : total;
  $('model-count').textContent = S.modelProvider
    ? t('models.countFiltered', { n: shown, total: total })
    : t('models.countAll', { n: total });
}

function renderModels() {
  const filter = S.modelFilter.trim().toLowerCase();
  // 两级过滤：先按自由文本收窄，再按上游收窄。分面计数建立在文本结果之上，
  // 标签上的数字就是「切过去真能看到几条」，不会出现点了没反应的死标签。
  const searched = (S.state.models || []).filter((m) => {
    if (!filter) return true;
    return (m.id + ' ' + (m.name || '') + ' ' + (m.provider || '')).toLowerCase().includes(filter);
  });
  const buckets = providerBuckets(searched);
  // 选中的上游在当前结果里已不存在（换了搜索词、上游被禁用等）时退回「全部」，
  // 否则列表会一直空着，用户看不出是被筛选条件挡掉了。
  if (S.modelProvider && !buckets.some((b) => b.id === S.modelProvider)) S.modelProvider = '';
  renderModelFilters(buckets, searched.length);
  const models = S.modelProvider ? searched.filter((m) => modelProviderID(m) === S.modelProvider) : searched;
  // 见 _modelTableSig 的注释：内容没变就不动表格，保住用户在单元格里的选中与焦点。
  const sig = models.length ? [I18N.lang, S.modelProvider, filter, JSON.stringify(models)].join('|') : '';
  if (sig && sig === _modelTableSig) return;
  _modelTableSig = sig;

  const body = $('model-body');
  body.innerHTML = '';
  if (!models.length) {
    const row = el('tr');
    const cell = el('td', 'empty', t('models.noMatch'));
    cell.colSpan = 7;
    row.appendChild(cell);
    body.appendChild(row);
    return;
  }
  models.forEach((m) => {
    const row = el('tr');
    row.appendChild(el('td', 'mono', m.id));
    // 虚拟组没有独立的展示名（后端只回组名），按当前语言补一个后缀说明，
    // 避免它在表格里看起来和真实模型毫无区别。
    const nameCell = el('td');
    nameCell.appendChild(document.createTextNode(m.name || '-'));
    if (m.virtual) nameCell.appendChild(el('span', 'tag on vm-tag', t('models.virtualTag')));
    row.appendChild(nameCell);
    const pcell = el('td');
    pcell.appendChild(el('span', 'tag on', m.provider || '-'));
    row.appendChild(pcell);
    row.appendChild(accountCell(m));
    row.appendChild(el('td', 'mono', formatTokens(m.context)));
    row.appendChild(el('td', 'mono', formatTokens(m.maxOut)));
    row.appendChild(el('td', null, m.images ? t('models.imgYes') : '—'));
    // 右键菜单：直接复制模型 ID，省得手动选中再复制。
    row.oncontextmenu = (e) => showModelMenu(e, m);
    body.appendChild(row);
  });
}

/* 可用账号数：这个模型当前有几个账号能承接请求（其余的已暂停或在冷却中）。 */
function accountCell(m) {
  const cell = el('td', 'mono');
  const total = m.totalAccounts || 0;
  const avail = m.accounts || 0;
  if (!total) {
    cell.appendChild(el('span', 'muted', '-'));
    return cell;
  }
  cell.appendChild(el('span', avail ? 'acct-ok' : 'acct-bad', String(avail)));
  cell.appendChild(el('span', 'muted', ' / ' + total));
  cell.title = t('models.accountsHint', { n: avail, total: total });
  return cell;
}

/* --------------------------- 模型右键菜单 --------------------------- */

function showModelMenu(e, m) {
  e.preventDefault();
  const menu = $('ctx-menu');
  menu.innerHTML = '';
  const selection = String(window.getSelection() || '').trim();

  const add = (label, value) => {
    const item = el('button', 'ctx-item', label);
    item.onclick = () => {
      hideModelMenu();
      copyRaw(value);
    };
    menu.appendChild(item);
  };
  // 用户在单元格里选中了文字时，优先给「复制选中内容」。
  if (selection) add(t('ctx.copySelection'), selection);
  add(t('ctx.copyId'), m.id);
  if (m.name && m.name !== m.id) add(t('ctx.copyName'), m.name);

  menu.classList.add('show');
  // 按菜单的真实尺寸贴边：写死 200/120 会在窄窗口或多条目时被裁掉一角，
  // 靠近右下角时改为向左上翻转。
  const w = menu.offsetWidth || 200;
  const h = menu.offsetHeight || 100;
  const x = e.clientX + w + 8 > window.innerWidth ? Math.max(4, e.clientX - w) : e.clientX;
  const y = e.clientY + h + 8 > window.innerHeight ? Math.max(4, e.clientY - h) : e.clientY;
  menu.style.left = x + 'px';
  menu.style.top = y + 'px';
}

function hideModelMenu() {
  $('ctx-menu').classList.remove('show');
}

async function copyRaw(value) {
  try {
    await call('CopyText', value);
    toast(t('toast.copied'));
  } catch (err) {
    toast(t('toast.copyFailedFmt', { err: String(err) }));
  }
}

/* --------------------------- 虚拟模型组 --------------------------- */

/* 上次渲染虚拟组列表的签名：数据没变就不重建，避免 20 秒轮询把用户
 * 正在点的按钮或刚展开的编辑区打断。 */
let _vmListSig = '';

function renderVirtualModels() {
  const list = $('vm-list');
  if (!list) return;
  const vms = (S.state && S.state.virtualModels) || [];
  // 真实模型 ID 集合：用来提示「组名已被某个真实模型占用」。
  const modelIDs = new Set(((S.state && S.state.models) || []).filter((m) => !m.virtual).map((m) => m.id));
  const sig = [I18N.lang, JSON.stringify(vms), Array.from(modelIDs).join(',')].join('|');
  if (sig !== _vmListSig) {
    _vmListSig = sig;
    list.innerHTML = '';
    if (!vms.length) {
      list.appendChild(el('div', 'empty', t('vm.empty')));
    }
    vms.forEach((vm) => {
      const item = el('div', 'vm-item' + (vm.enabled ? '' : ' off'));
      const head = el('div', 'vm-item-head');
      head.appendChild(el('span', 'vm-item-name', vm.name));

      const ops = el('div', 'ops');
      // 可用成员数：0 表示这个组此刻一条路都走不通，用警示色标出来。
      const total = (vm.members || []).length;
      ops.appendChild(el('span', 'tag' + (vm.available > 0 ? ' on' : ' warn'),
        t('vm.availableFmt', { n: vm.available, total: total })));
      ops.appendChild(el('span', 'tag' + (vm.enabled ? ' on' : ''), vm.enabled ? t('tag.enabled') : t('tag.disabled')));
      // 组名与某个真实模型 ID 相同时，模型页会显示真实模型（列表里不重复列出），
      // 这里说明一下，免得用户以为自己的组没生效。
      if (modelIDs.has(vm.name)) {
        ops.appendChild(el('span', 'tag warn', t('vm.nameTaken')));
      }

      const edit = el('button', 'btn btn-mini', t('btn.edit'));
      edit.onclick = () => openVirtualEditor(vm.name);
      ops.appendChild(edit);

      const del = el('button', 'btn btn-mini btn-danger', t('btn.delete'));
      del.onclick = () => deleteVirtualModel(vm.name);
      ops.appendChild(del);
      head.appendChild(ops);
      item.appendChild(head);

      const members = el('div', 'vm-item-members');
      (vm.members || []).forEach((m) => {
        // 不可用的成员划掉：用户一眼能看出「配了 3 个，实际只有 2 个在跑」。
        const chip = el('span', 'vm-member-chip' + (m.available ? '' : ' down'),
          m.providerName + ' · ' + m.model);
        if (m.weight > 1) chip.appendChild(el('span', 'w', '×' + m.weight));
        // 上游被全局停用但成员仍可用：标一下，免得用户以为设置没生效。
        if (m.providerOff) {
          chip.appendChild(el('span', 'w', t('vm.providerOff')));
          chip.title = t('vm.providerOffTip');
        }
        members.appendChild(chip);
      });
      if (total === 0) members.appendChild(el('span', 'hint', t('vm.noMembers')));
      item.appendChild(members);
      list.appendChild(item);
    });
  }
  // 正在编辑的组若已不存在（被另一个窗口删除、配置被手改），编辑器要收起来：
  // 否则保存时会报「不存在」，而用户看着一个正常打开的编辑面板无法理解。
  if (S.vm.editing && !vms.some((v) => v.name === S.vm.editing)) {
    closeVirtualEditor();
    toast(t('vm.editor.vanished'));
  }
  renderVirtualEditorSelects();
}

/* 编辑器里的「上游 → 模型」下拉：列出所有已登录账号的上游。
 *
 * 不只列已启用的：虚拟组是用户对「用哪个上游的哪个模型」的显式选择，
 * 优先级高于上游的全局开关。只列已启用的会把「登录了但没启用」的上游
 * 整个藏起来，用户明明有账号却选不到。
 * 未启用或未登录的上游仍列出来，但标注原因并禁用，避免用户找不到。 */
function renderVirtualEditorSelects() {
  const pSel = $('vm-add-provider');
  if (!pSel) return;
  const providers = (S.state && S.state.providers) || [];
  const sig = [I18N.lang, providers.map((p) => p.id + ':' + p.name + ':' + p.accountCount + ':' + p.enabled).join(',')].join('|');
  if (pSel._sig !== sig) {
    pSel._sig = sig;
    const prev = pSel.value;
    pSel.innerHTML = '';
    providers.forEach((p) => {
      const o = el('option');
      o.value = p.id;
      // 有账号就能选；没账号的保留但禁用，并说明原因。
      const usable = p.accountCount > 0;
      let suffix = '';
      if (!usable) suffix = ' ' + t('vm.notLogged');
      else if (!p.enabled) suffix = ' ' + t('vm.providerOff');
      o.textContent = p.name + suffix;
      o.disabled = !usable;
      pSel.appendChild(o);
    });
    const firstUsable = providers.find((p) => p.accountCount > 0);
    pSel.value = (prev && providers.some((p) => p.id === prev && p.accountCount > 0))
      ? prev
      : (firstUsable ? firstUsable.id : '');
  }
  renderVirtualModelOptions();
}

/* 按当前选中的上游填充模型下拉。
 *
 * 走 GetProviderModels 而不是从 state.models 里筛：state.models 只含
 * 已启用上游的模型，而虚拟组允许用「已登录但未全局启用」的上游，
 * 那些模型在 state.models 里根本不存在。
 * 接口返回的就是该上游的原始模型 ID，与成员存储形式一致，无需去前缀。 */
let _vmModelReq = 0;
async function renderVirtualModelOptions() {
  const pSel = $('vm-add-provider');
  const mSel = $('vm-add-model');
  if (!pSel || !mSel) return;
  const pid = pSel.value;
  if (!pid) {
    mSel.innerHTML = '';
    mSel._sig = '';
    return;
  }
  // 连续切换上游时只认最后一次请求的结果，避免慢响应把新选择覆盖掉。
  const req = ++_vmModelReq;
  let models = [];
  let failed = false;
  try {
    models = await call('GetProviderModels', pid);
  } catch (err) {
    failed = true;
    models = [];
  }
  if (req !== _vmModelReq || pSel.value !== pid) return;

  // 请求失败不写签名：签名一写就等于把「这次失败」缓存下来，用户切走再切回
  // 同一个上游也不会重试。而且「没有可用模型」与「读取失败」是两回事，
  // 前者可以继续操作，后者应当提示去检查登录状态。
  if (failed) {
    mSel.innerHTML = '';
    mSel._sig = '';
    const o = el('option');
    o.value = '';
    o.textContent = t('vm.modelsLoadFailed');
    o.disabled = true;
    mSel.appendChild(o);
    return;
  }

  const sig = [pid, (models || []).join(',')].join('|');
  if (mSel._sig === sig) return;
  mSel._sig = sig;
  mSel.innerHTML = '';
  (models || []).forEach((id) => {
    const o = el('option');
    o.value = id;
    o.textContent = id;
    mSel.appendChild(o);
  });
  if (!models || !models.length) {
    const o = el('option');
    o.value = '';
    o.textContent = t('vm.noModels');
    o.disabled = true;
    mSel.appendChild(o);
  }
}


function openVirtualEditor(name) {
  const vms = (S.state && S.state.virtualModels) || [];
  const vm = name ? vms.find((v) => v.name === name) : null;
  S.vm.editing = vm ? vm.name : '';
  S.vm.members = vm
    ? vm.members.map((m) => ({ provider: m.provider, providerName: m.providerName, model: m.model, weight: m.weight }))
    : [];
  $('vm-editor').classList.remove('hidden');
  $('vm-editor-title').textContent = vm ? t('vm.editor.edit') : t('vm.editor.new');
  $('vm-editor-hint').textContent = vm ? t('vm.editor.editHint') : '';
  $('vm-name').value = vm ? vm.name : '';
  $('vm-enabled').checked = vm ? vm.enabled : true;
  $('vm-editor-msg').textContent = '';
  renderVirtualMembers();
  renderVirtualEditorSelects();
  $('vm-name').focus();
}

function closeVirtualEditor() {
  $('vm-editor').classList.add('hidden');
  S.vm.editing = '';
  S.vm.members = [];
}

function renderVirtualMembers() {
  const wrap = $('vm-members');
  if (!wrap) return;
  wrap.innerHTML = '';
  if (!S.vm.members.length) {
    wrap.appendChild(el('div', 'empty', t('vm.editor.noMembers')));
    return;
  }
  S.vm.members.forEach((m, index) => {
    const row = el('div', 'vm-member-row');
    const who = el('span', 'who');
    who.appendChild(document.createTextNode(m.providerName + ' · '));
    who.appendChild(el('em', null, m.model));
    row.appendChild(who);

    const weight = el('input', 'input weight');
    weight.type = 'number';
    weight.min = '1';
    weight.value = String(m.weight || 1);
    weight.title = t('vm.weight');
    weight.oninput = () => {
      S.vm.members[index].weight = Math.max(1, parseInt(weight.value, 10) || 1);
    };
    row.appendChild(weight);

    const del = el('button', 'btn btn-mini btn-danger', t('btn.delete'));
    del.onclick = () => {
      S.vm.members.splice(index, 1);
      renderVirtualMembers();
    };
    row.appendChild(del);
    wrap.appendChild(row);
  });
}

function addVirtualMember() {
  const pSel = $('vm-add-provider');
  const pid = pSel.value;
  const model = $('vm-add-model').value;
  // 区分「还没选」和「压根没有可选上游」：后者是登录/启用状态问题，
  // 提示「请先选择」会让用户在下拉里反复找不存在的选项。
  if (!pSel.options.length) return toast(t('vm.editor.noProvider'));
  if (!pid || !model) return toast(t('vm.editor.pickFirst'));
  // 同一 (上游, 模型) 只允许出现一次：重复加入不会让流量翻倍，
  // 只会让权重语义变得难以理解。
  if (S.vm.members.some((m) => m.provider === pid && m.model === model)) {
    return toast(t('vm.editor.duplicate'));
  }
  const provider = ((S.state && S.state.providers) || []).find((p) => p.id === pid);
  S.vm.members.push({
    provider: pid,
    providerName: provider ? provider.name : pid,
    model: model,
    weight: Math.max(1, parseInt($('vm-add-weight').value, 10) || 1),
  });
  renderVirtualMembers();
}

async function saveVirtualModel() {
  const name = $('vm-name').value.trim();
  if (!name) return toast(t('vm.editor.nameRequired'));
  // 与后端 config.ValidVirtualName 保持同一套规则：: 和 / 是强制路由分隔符，
  // 空白会让日志与统计难以辨认，intl 是国际版上游的别名。
  if (/[:/\s]/.test(name)) return toast(t('vm.editor.badName'));
  if (name.toLowerCase() === 'intl') return toast(t('vm.editor.reservedName'));
  if (!S.vm.members.length) return toast(t('vm.editor.needMember'));

  const input = {
    name: name,
    enabled: $('vm-enabled').checked,
    originalName: S.vm.editing,
    members: S.vm.members.map((m) => ({ provider: m.provider, model: m.model, weight: m.weight })),
  };
  $('btn-vm-save').disabled = true;
  $('vm-editor-msg').textContent = t('save.saving');
  try {
    await call('SaveVirtualModel', input);
    $('vm-editor-msg').textContent = '';
    toast(t('vm.saved'));
    closeVirtualEditor();
    await refresh();
  } catch (err) {
    const msg = t('toast.opFailedFmt', { err: String(err) });
    $('vm-editor-msg').textContent = msg;
    toast(msg);
  } finally {
    $('btn-vm-save').disabled = false;
  }
}

async function deleteVirtualModel(name) {
  if (!confirm(t('vm.confirmDeleteFmt', { name: name }))) return;
  try {
    await call('DeleteVirtualModel', name);
    toast(t('vm.deleted'));
    if (S.vm.editing === name) closeVirtualEditor();
    await refresh();
  } catch (err) {
    toast(t('toast.opFailedFmt', { err: String(err) }));
  }
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

/* --------------------------- 流量统计 --------------------------- */

/* 构建「组 → 模型」的下拉选项：来源既包含已同步的模型（S.state.models），
 * 也包含历史上产生过流量的 (组, 模型) 组合（scopes），这样即使某模型当前被停用，
 * 也能回看它的历史流量。
 *
 * 虚拟模型组作为独立的组列出（值形如 "virtual:<组名>"）：它有自己的流量维度，
 * 与真实上游并列供用户切换观察。 */
function buildStatsOptions(scopes) {
  const providers = [];
  const modelsByProvider = {};
  const seenP = {};
  const seenM = {};

  const ensureProvider = (id, label) => {
    if (!id) return;
    if (!modelsByProvider[id]) modelsByProvider[id] = [];
    if (!seenP[id]) { providers.push({ value: id, label: label || id }); seenP[id] = true; }
  };
  const ensureModel = (pid, mid) => {
    if (!pid || !mid) return;
    ensureProvider(pid, pid);
    const key = pid + '|' + mid;
    if (!seenM[key]) { modelsByProvider[pid].push({ value: mid, label: mid }); seenM[key] = true; }
  };

  (S.state && S.state.providers || []).forEach((p) => ensureProvider(p.id, p.name));
  (S.state && S.state.models || []).forEach((m) => {
    if (m.virtual) return; // 虚拟组的成员明细来自它自己的维度，不混进真实上游
    ensureModel(m.providerId || m.provider, m.id);
  });
  // 已配置的虚拟组即使还没有流量也要出现在下拉里：否则用户新建一个组后
  // 在统计页根本选不到它，看到的是「这个组不存在」而不是「它还没跑过请求」。
  (S.state && S.state.virtualModels || []).forEach((vm) => {
    ensureProvider(virtualStatsValue(vm.name), t('stats.virtualFmt', { name: vm.name }));
  });
  (scopes || []).forEach((s) => {
    if (s.kind === 'virtual') {
      // 虚拟组只作为一个整体出现，model 恒为空（成员明细单独展示）。
      ensureProvider(virtualStatsValue(s.provider), t('stats.virtualFmt', { name: s.provider }));
      return;
    }
    ensureModel(s.provider, s.model);
  });

  providers.sort((a, b) => {
    // 真实上游在前、虚拟组在后，与模型页的呈现顺序一致。
    const av = a.value.startsWith('virtual:');
    const bv = b.value.startsWith('virtual:');
    if (av !== bv) return av ? 1 : -1;
    return a.label.localeCompare(b.label);
  });
  return { providers: providers, modelsByProvider: modelsByProvider };
}

/* 统计页里虚拟组的取值前缀：与后端 stats.VirtualKey 保持一致。 */
const VIRTUAL_STATS_PREFIX = 'virtual:';
const virtualStatsValue = (name) => VIRTUAL_STATS_PREFIX + name;

/* 用 items 重建 select，尽量保留 sel 当前选中的值（若仍存在于新选项里）。 */
function fillStatsSelect(sel, items, selectedValue, allLabel) {
  const prev = selectedValue !== undefined ? selectedValue : sel.value;
  sel.innerHTML = '';
  if (allLabel) {
    const o = el('option');
    o.value = '';
    o.textContent = allLabel;
    sel.appendChild(o);
  }
  items.forEach((it) => {
    const o = el('option');
    o.value = it.value;
    o.textContent = it.label;
    sel.appendChild(o);
  });
  if (prev !== undefined && Array.prototype.some.call(sel.options, (o) => o.value === prev)) {
    sel.value = prev;
  } else if (allLabel) {
    sel.value = '';
  }
}

/* 只有选项集合真的变了才重建 select：定时刷新时重复重建会把用户展开的下拉打断。 */
function syncStatsSelect(sel, items, selectedValue, allLabel) {
  const sig = (allLabel || '') + '|' + items.map((it) => it.value + ':' + it.label).join('|');
  if (sel._sig !== sig) {
    fillStatsSelect(sel, items, selectedValue, allLabel);
    sel._sig = sig;
  } else if (selectedValue !== undefined && sel.value !== selectedValue) {
    sel.value = selectedValue;
  }
}

/* 流量统计的自动刷新周期。后端 Query 是纯内存聚合（internal/stats.Recorder
 * 收到请求即累加），3 秒一次的代价可以忽略。 */
const STATS_REFRESH_MS = 3000;
let _statsTimer = null;
let _statsRunning = false;
let _statsPending = false;
let _statsSig = '';      // 最近一次实际渲染用的完整签名（含数据），用于跳过无变化重绘
let _statsFilter = '';   // 最近一次成功取数时的筛选签名，用于判断能否沿用上次画面

/* 统计页是否处于前台：标签页激活且窗口可见（最小化 / 隐藏到托盘都算不可见）。 */
function statsTabActive() {
  const tab = $('tab-stats');
  return !!(tab && tab.classList.contains('active')) && !document.hidden;
}

function stopStatsAutoRefresh() {
  if (!_statsTimer) return;
  clearInterval(_statsTimer);
  _statsTimer = null;
}

/* 让自动刷新与当前状态对齐：切标签页、窗口 focus / visibilitychange 时都调它。
 * 在跑且该跑 → 空操作（重复的 focus / visibilitychange 不会重复拉取）；
 * 该跑但没在跑 → 先补一次即时刷新再启动，切换过来不必空等一个周期；
 * 不该跑（离开该页 / 窗口不可见）→ 停掉。 */
function syncStatsAutoRefresh() {
  if (!statsTabActive()) return stopStatsAutoRefresh();
  if (_statsTimer) return;
  renderStats();
  _statsTimer = setInterval(() => {
    if (!statsTabActive()) return stopStatsAutoRefresh();
    renderStats();
  }, STATS_REFRESH_MS);
}

/* 定时轮询、切换筛选、全局状态刷新可能同时要渲染统计：串行成一次，避免互相覆盖。 */
function renderStats() {
  if (_statsRunning) { _statsPending = true; return; }
  _statsRunning = true;
  renderStatsOnce()
    .catch(() => { /* 单次失败交给下一轮重试，不弹提示打扰用户 */ })
    .finally(() => {
      _statsRunning = false;
      if (_statsPending) { _statsPending = false; renderStats(); }
    });
}

async function renderStatsOnce() {
  const providerSel = $('stats-provider');
  const modelSel = $('stats-model');
  if (!providerSel || !modelSel) return;

  let scopes = [];
  try { scopes = await call('GetTrafficScopes'); } catch (err) { scopes = []; }

  const opts = buildStatsOptions(scopes);
  if (!S.stats.provider) S.stats.provider = (opts.providers[0] && opts.providers[0].value) || '';
  syncStatsSelect(providerSel, opts.providers, S.stats.provider);
  S.stats.provider = providerSel.value;

  // 虚拟组没有「按成员筛选」的下拉（成员明细单独一张表），把模型选择禁用并
  // 只留「全部模型」一项，避免出现一个永远选不出东西的空下拉让人以为坏了。
  // 必须在填充模型下拉之前判断：否则会先塞进一批真实模型再禁用，视觉上闪一下。
  const isVirtual = S.stats.provider.startsWith(VIRTUAL_STATS_PREFIX);
  modelSel.disabled = isVirtual;
  if (isVirtual) {
    S.stats.model = '';
    syncStatsSelect(modelSel, [], '', t('stats.allModels'));
  } else {
    const models = opts.modelsByProvider[S.stats.provider] || [];
    syncStatsSelect(modelSel, models, S.stats.model, t('stats.allModels'));
    S.stats.model = modelSel.value;
  }

  // 当前筛选条件。取数失败时用它判断能否沿用上次画面：筛选没变才保留，
  // 否则界面会「显示周、画着日」，比闪烁更误导。
  const filter = [S.stats.provider, S.stats.model, S.stats.period].join('|');

  let res;
  try {
    res = await call('GetTrafficStats', S.stats.provider, S.stats.model, S.stats.period);
  } catch (err) {
    // 单次查询失败（后端忙、服务重启中）不该清空已经画好的内容：3 秒一轮的
    // 刷新下，清空重画就是肉眼可见的闪烁。筛选条件没变就保留上次结果，下一轮
    // 自动重试；筛选变了或首次加载就失败，则退回「暂无数据」提示。
    if (filter === _statsFilter) return;
    res = null;
  }

  // 筛选、语言、数据都没变就跳过重绘，否则几秒一次的刷新会让图表和卡片反复闪。
  const sig = [I18N.lang, filter, JSON.stringify(res)].join('|');
  if (sig === _statsSig) return;
  _statsSig = sig;
  if (res) _statsFilter = filter;

  if (!res) {
    $('stats-range').textContent = '';
    $('stats-cards').innerHTML = '';
    $('stats-chart').innerHTML = '';
    $('stats-empty').textContent = t('stats.noData');
    $('stats-empty').classList.remove('hidden');
    renderStatsMembers(null);
    return;
  }

  const empty = !res.series || res.series.length === 0 ||
    res.series.every((p) => !p.in && !p.out && !p.total);
  $('stats-empty').classList.toggle('hidden', !empty);
  $('stats-chart').classList.toggle('hidden', empty);
  // 成员分摊表独立于周期趋势：它展示的是全时段累计，即使当前周期
  // 没有流量也照样有意义（用户切到「日」时仍想知道长期分摊是否均衡）。
  renderStatsMembers(res.members);
  if (empty) {
    $('stats-cards').innerHTML = '';
    $('stats-chart').innerHTML = '';
    $('stats-range').textContent = '';
    return;
  }
  renderStatCards(res.summary);
  $('stats-range').textContent = (res.startLabel && res.endLabel)
    ? (res.startLabel + '  ' + t('stats.to') + '  ' + res.endLabel)
    : '';
  renderTrafficChart($('stats-chart'), res);
}

/* 渲染虚拟组的成员分摊表；members 为空（或不是虚拟组）时整张卡片隐藏。 */
function renderStatsMembers(members) {
  const card = $('stats-members-card');
  const body = $('stats-members-body');
  if (!card || !body) return;
  if (!members || !members.length) {
    card.classList.add('hidden');
    return;
  }
  card.classList.remove('hidden');
  body.innerHTML = '';
  const total = members.reduce((acc, m) => acc + (m.total || 0), 0);
  members.forEach((m) => {
    const row = el('tr');
    row.appendChild(el('td', 'mono', m.provider + ' · ' + m.model));
    row.appendChild(el('td', 'mono', String(m.count || 0)));
    row.appendChild(el('td', 'mono', formatTokens(m.in)));
    row.appendChild(el('td', 'mono', formatTokens(m.out)));
    row.appendChild(el('td', 'mono', formatTokens(m.total)));
    const share = total > 0 ? Math.round(((m.total || 0) / total) * 100) : 0;
    row.appendChild(el('td', 'mono', share + '%'));
    body.appendChild(row);
  });
}

function renderStatCards(sum) {
  const wrap = $('stats-cards');
  wrap.innerHTML = '';
  const cards = [
    { key: 'stats.in', value: formatTokens(sum.in), cls: 'in' },
    { key: 'stats.out', value: formatTokens(sum.out), cls: 'out' },
    { key: 'stats.total', value: formatTokens(sum.total), cls: 'total' },
    { key: 'stats.count', value: String(sum.count ?? '-'), cls: 'count' },
  ];
  cards.forEach((c) => {
    const card = el('div', 'stat-card accent-' + c.cls);
    card.appendChild(el('div', 'label', t(c.key)));
    card.appendChild(el('div', 'value', c.value));
    wrap.appendChild(card);
  });
}

/* 手绘 SVG 折线图：输入 / 输出 / 总计 三条线 + 面积，自适应明暗主题。
 * 不使用任何图表库，纯 DOM/SVG，避免引入前端依赖。 */
function renderTrafficChart(container, res) {
  container.innerHTML = '';
  const series = res.series || [];
  if (!series.length) return;
  const hasData = series.some((p) => p.in || p.out || p.total);
  if (!hasData) return;

  const W = 680, H = 260;
  const PL = 46, PR = 14, PT = 16, PB = 28;
  const plotW = W - PL - PR;
  const plotH = H - PT - PB;
  const n = series.length;
  const xAt = (i) => (n === 1 ? PL + plotW / 2 : PL + (plotW * i) / (n - 1));

  let maxV = 0;
  series.forEach((p) => { maxV = Math.max(maxV, p.in, p.out, p.total); });
  if (maxV <= 0) return;
  const step = niceStep(maxV / 4);
  const topVal = Math.ceil(maxV / step) * step;
  const yAt = (v) => PT + plotH * (1 - v / topVal);

  const NS = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('viewBox', '0 0 ' + W + ' ' + H);
  svg.setAttribute('width', '100%');
  svg.setAttribute('preserveAspectRatio', 'xMidYMid meet');

  // 横向网格 + Y 轴刻度（自适应主题，用 CSS 变量着色）
  const lines = 4;
  for (let g = 0; g <= lines; g++) {
    const v = (topVal * g) / lines;
    const y = yAt(v);
    const ln = document.createElementNS(NS, 'line');
    ln.setAttribute('x1', PL); ln.setAttribute('x2', W - PR);
    ln.setAttribute('y1', y); ln.setAttribute('y2', y);
    ln.style.stroke = 'var(--border)';
    ln.style.strokeWidth = '1';
    svg.appendChild(ln);
    const tx = document.createElementNS(NS, 'text');
    tx.setAttribute('x', PL - 6); tx.setAttribute('y', y + 3);
    tx.setAttribute('text-anchor', 'end');
    tx.style.fill = 'var(--text-faint)';
    tx.style.fontSize = '10px';
    tx.textContent = formatTokens(Math.round(v));
    svg.appendChild(tx);
  }

  // X 轴标签（最多约 8 个，避免拥挤）
  const labelStep = Math.max(1, Math.ceil(n / 8));
  series.forEach((p, i) => {
    if (i % labelStep !== 0 && i !== n - 1) return;
    const tx = document.createElementNS(NS, 'text');
    tx.setAttribute('x', xAt(i)); tx.setAttribute('y', H - 8);
    tx.setAttribute('text-anchor', 'middle');
    tx.style.fill = 'var(--text-faint)';
    tx.style.fontSize = '10px';
    tx.textContent = p.label ?? '';
    svg.appendChild(tx);
  });

  const defs = [
    { key: 'in', color: 'var(--accent)', fill: 'rgba(59,130,246,0.10)' },
    { key: 'out', color: 'var(--accent-violet)', fill: 'rgba(139,92,246,0.10)' },
    { key: 'total', color: '#10b981', fill: 'rgba(16,185,129,0.08)' },
  ];
  const showDots = n <= 16;

  defs.forEach((d) => {
    let area = xAt(0) + ',' + yAt(0);
    series.forEach((p, i) => { area += ' ' + xAt(i) + ',' + yAt(p[d.key]); });
    area += ' ' + xAt(n - 1) + ',' + yAt(0);
    const polyArea = document.createElementNS(NS, 'polygon');
    polyArea.setAttribute('points', area);
    polyArea.style.fill = d.fill;
    svg.appendChild(polyArea);

    let pts = '';
    series.forEach((p, i) => { pts += xAt(i) + ',' + yAt(p[d.key]) + ' '; });
    const poly = document.createElementNS(NS, 'polyline');
    poly.setAttribute('points', pts.trim());
    poly.style.fill = 'none';
    poly.style.stroke = d.color;
    poly.style.strokeWidth = d.key === 'total' ? '2.4' : '1.8';
    poly.style.strokeLinejoin = 'round';
    poly.style.strokeLinecap = 'round';
    svg.appendChild(poly);

    if (showDots) {
      series.forEach((p, i) => {
        const c = document.createElementNS(NS, 'circle');
        c.setAttribute('cx', xAt(i)); c.setAttribute('cy', yAt(p[d.key])); c.setAttribute('r', '2.2');
        c.style.fill = d.color;
        svg.appendChild(c);
      });
    }
  });

  // 图例（HTML，便于多语言与配色说明）
  const legend = el('div', 'chart-legend');
  [
    ['var(--accent)', t('stats.in')],
    ['var(--accent-violet)', t('stats.out')],
    ['#10b981', t('stats.total')],
  ].forEach((pair) => {
    const item = el('span', 'legend-item');
    const chip = el('span', 'legend-chip');
    chip.style.background = pair[0];
    item.appendChild(chip);
    item.appendChild(el('span', null, pair[1]));
    legend.appendChild(item);
  });

  container.appendChild(legend);
  container.appendChild(svg);
}

/* 把任意正数映射成「整齐」的坐标轴步长（1/2/5 × 10^n）。 */
function niceStep(raw) {
  if (!(raw > 0)) return 1;
  const mag = Math.pow(10, Math.floor(Math.log10(raw)));
  const norm = raw / mag;
  let nice;
  if (norm <= 1) nice = 1;
  else if (norm <= 2) nice = 2;
  else if (norm <= 5) nice = 5;
  else nice = 10;
  return nice * mag;
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
  renderVirtualModels();
  renderSettings();
  // 流量统计不走这里：它由自己的定时器负责（见 syncStatsAutoRefresh），
  // 只在统计页激活时每几秒刷新一次。
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

let _refreshing = false;
async function refresh() {
  if (_refreshing) return; // 防止轮询与手动刷新重叠导致重复渲染 / 竞态覆盖 S.state
  _refreshing = true;
  try {
    S.state = await call('GetState');
    S.logs = S.state.logs || [];
    S.login = S.state.login || { status: 'idle' };
    renderAll();
  } catch (err) {
    toast(t('toast.stateFailedFmt', { err: String(err) }));
  } finally {
    _refreshing = false;
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

/* 账号类操作的统一包装：成功提示并刷新，失败提示原因。 */
async function guard(_target, run, okMessage) {
  try {
    await run();
    toast(okMessage);
    await refresh();
  } catch (err) {
    toast(t('toast.opFailedFmt', { err: String(err) }));
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
    proxy: (Array.from(document.querySelectorAll('#provider-config input[type=text]')).find((i) => i.dataset.provider === c.dataset.provider) || {}).value || '',
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
      // 启停统一交给 syncStatsAutoRefresh 收敛：切到统计页启动，切走停止
      syncStatsAutoRefresh();
    };
  });

  document.querySelectorAll('[data-copy]').forEach((btn) => {
    btn.onclick = () => copyFrom($(btn.dataset.copy));
  });

  // 右键菜单：点空白处、滚动、缩放时收起。
  // 菜单是 fixed 定位的，表格滚动时它会原地不动、脱离原来指向的那一行，
  // 所以滚动也必须收起。
  document.addEventListener('click', hideModelMenu);
  document.addEventListener('contextmenu', (e) => {
    if (!e.target.closest('#model-body tr')) hideModelMenu();
  });
  document.addEventListener('scroll', hideModelMenu, true);
  window.addEventListener('blur', hideModelMenu);
  window.addEventListener('resize', hideModelMenu);

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
    // 授权类上游打开登录页；API Key 制上游没有授权链接，改为打开控制台。
    const target = S.login.url || providerConsoleURL(S.login.provider);
    if (target) call('OpenURL', target).catch(() => {});
  };
  // 登录输入提交（粘贴授权码 / 粘贴密钥）：Enter 与按钮等价。失败时保留
  // 输入框内容便于重试，刷新后仍处于 pending 会重新放开按钮。提交期间后端
  // 要联机校验，用 submittingKey 挡住轮询刷新与重复点击。
  //
  // 授权码（needCode）交给 SubmitLoginCode，由后台轮询完成 token 交换；
  // 密钥交给 SubmitLoginKey。本机导入会话（state === 'local'）允许空提交：
  // 空提交 = 确认使用本机已保存的密钥——导入不再自动登录，用户有机会先粘贴
  // 新密钥。
  $('btn-submit-key').onclick = async () => {
    if (submittingKey) return;
    const provider = S.login.provider;
    const needCode = !!S.login.needCode;
    const localImport = !needCode && S.login.state === 'local';
    const value = $('login-key').value.trim();
    if (!value && !localImport) return toast(t('toast.keyEmpty'));
    submittingKey = true;
    $('btn-submit-key').disabled = true;
    let ok = false;
    try {
      if (needCode) {
        await call('SubmitLoginCode', provider, value);
      } else {
        await call('SubmitLoginKey', provider, value);
      }
      ok = true;
    } catch (err) {
      toast(t('toast.opFailedFmt', { err: String(err) }));
    }
    submittingKey = false;
    // 授权码只是「交给后台换 token」，成功与否由轮询定论：这里不清空输入框，
    // 万一后台换 token 失败（复制漏字符很常见），用户还能改一两个字符重试。
    // 密钥提交是同步校验的，成功即可清空。
    if (ok && !needCode) $('login-key').value = '';
    await refresh();
  };
  $('login-key').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') $('btn-submit-key').click();
  });
  $('model-search').oninput = (e) => { S.modelFilter = e.target.value; renderModels(); };

  // 语言切换即时生效；其余设置项保持「编辑后点保存」的节奏
  $('set-language').onchange = (e) => changeLanguage(e.target.value);
  document.querySelectorAll('#tab-settings input, #tab-settings select').forEach((node) => {
    if (node.id === 'set-language') return;
    node.addEventListener('input', () => { S.dirty = true; });
    node.addEventListener('change', () => { S.dirty = true; });
  });
  $('btn-save').onclick = saveSettings;

  // 虚拟模型组
  $('btn-vm-new').onclick = () => openVirtualEditor('');
  $('btn-vm-cancel').onclick = () => closeVirtualEditor();
  $('btn-vm-save').onclick = () => saveVirtualModel();
  $('btn-vm-add-member').onclick = () => addVirtualMember();
  $('vm-add-provider').onchange = () => {
    // 换上游后模型列表整批变化，强制重建（清掉签名）。
    const mSel = $('vm-add-model');
    if (mSel) mSel._sig = '';
    renderVirtualModelOptions();
  };

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

  // 流量统计：组 / 模型 / 周期切换
  $('stats-provider').onchange = () => { S.stats.provider = $('stats-provider').value; S.stats.model = ''; renderStats(); };
  $('stats-model').onchange = () => { S.stats.model = $('stats-model').value; renderStats(); };
  document.querySelectorAll('#stats-period .seg-btn').forEach((b) => {
    b.onclick = () => {
      document.querySelectorAll('#stats-period .seg-btn').forEach((x) => x.classList.remove('active'));
      b.classList.add('active');
      S.stats.period = b.dataset.period;
      renderStats();
    };
  });

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
  // 窗口从最小化 / 托盘恢复、或重新可见时，若正停在统计页就立即续上刷新；
  // 已在跑时 syncStatsAutoRefresh 是空操作，focus 与 visibilitychange 先后
  // 触发也不会重复拉取
  window.addEventListener('focus', syncStatsAutoRefresh);
  document.addEventListener('visibilitychange', syncStatsAutoRefresh);
}

window.addEventListener('DOMContentLoaded', main);
