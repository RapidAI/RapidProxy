/* RapidProxy 界面多语言（简体中文 / English）。
 *
 * 用法：
 *   I18N.t(key, params)          取当前语言文案，{name} 占位符按 params 替换
 *   I18N.apply(root?)            把 data-i18n / data-i18n-ph / data-i18n-title
 *                                标注的静态文案整体刷新（含 <html lang>）
 *   I18N.setPref(pref)           设置语言偏好：'system' | 'zh' | 'en'
 *   I18N.pref / I18N.lang        当前偏好与实际语言（只读）
 *
 * 偏好存放在 Go 侧 config.json（设置页切换），这里额外缓存一份到
 * localStorage，让下次启动时在拿到后端状态之前就能用对语言，避免闪变。
 */
(function () {
  'use strict';

  var DICT = {
    /* ------------------------------ 简体中文 ------------------------------ */
    zh: {
      /* 外壳与导航 */
      'brand.subtitle': 'OpenAI 兼容网关',
      'nav.overview': '概览',
      'nav.accounts': '账号',
      'nav.models': '模型',
      'nav.settings': '设置',
      'nav.about': '关于',
      'btn.quit': '退出程序',
      'btn.resetWindow': '重置窗口',
      'btn.resetWindow.tip': '窗口超出屏幕时把窗口缩小并居中',
      'btn.hide': '最小化到托盘',
      'btn.start': '启动服务',
      'btn.stop': '停止服务',
      'status.running': '运行中',
      'status.stopped': '已停止',
      'status.serviceDown': '服务未启动',

      /* 概览 */
      'ov.access.title': '接入信息',
      'ov.access.hint': '把下面两项填入任意支持自定义 API 的软件',
      'btn.copy': '复制',
      'btn.show': '显示',
      'btn.hideKey': '隐藏',
      'key.regen': '重新生成',
      'key.gen': '生成密钥',
      'key.none': '未设置（任何密钥均可访问）',
      'ov.requireKey.yes': '客户端需要在请求头携带该密钥',
      'ov.requireKey.no': '当前不校验密钥，便于本地调试',
      'label.models': '模型列表',
      'label.sample': '调用示例',
      'btn.copySample': '复制示例',
      'ov.status.title': '服务状态',
      'ov.dt.running': '运行状态',
      'ov.dt.listen': '监听地址',
      'ov.dt.models': '可用模型',
      'ov.dt.accounts': '已登录账号',
      'ov.dt.sync': '模型同步间隔',
      'btn.syncNow': '立即同步模型',
      'btn.openDataDir': '打开数据目录',
      'ov.providers.title': '上游',
      'ov.logs.title': '运行日志',
      'btn.clear': '清空',
      'tag.enabled': '已启用',
      'tag.disabled': '未启用',
      'tag.logged': '已登录 · {n} 账号',
      'tag.notLogged': '未登录',
      'tag.models': '{n} 模型',
      'count.unit': '{n} 个',
      'ov.notLoggedIn': '尚未登录',
      'sync.every': '每 {n} 小时',
      'sync.off': '已关闭',

      /* 账号 */
      'acc.login.title': '登录账号',
      'acc.login.hint': '登录后即可把该平台背后的模型作为 OpenAI 接口使用',
      'btn.loginFmt': '登录 {name}',
      'acc.loginDisabled': '（未启用，登录后可到设置里启用）',
      'login.waiting': '等待登录',
      'login.pending': '等待在浏览器中完成授权',
      'login.success': '登录成功',
      'login.failed': '登录失败',
      'login.browserTip': '已在你系统的默认浏览器中打开官方登录页（Google / GitHub / 微信扫码）。在浏览器里完成授权后，本程序会自动感知并保存凭据；若浏览器没有自动打开，可复制下方链接手动访问。',
      'btn.copyLink': '复制链接',
      'btn.reopenLogin': '重新打开登录页',
      'btn.cancel': '取消',
      'login.expiresFmt': '链接有效期至 {time}',
      'acc.list.title': '已登录账号',
      'acc.countFmt': '{n} 个账号',
      'acc.empty': '还没有登录任何账号，点击上方按钮开始登录',
      'acc.uidUnknown': '未知',
      'acc.signedAtFmt': '登录于 {time}',
      'acc.expiresUnknown': '有效期未知',
      'btn.delete': '删除',

      /* 模型 */
      'models.title': '可用模型',
      'models.search': '搜索模型…',
      'btn.sync': '立即同步',
      'th.id': '模型 ID',
      'th.name': '名称',
      'th.provider': '上游',
      'th.context': '上下文',
      'th.maxOut': '最大输出',
      'th.images': '图片',
      'models.noMatch': '没有匹配的模型',
      'models.imgYes': '支持',

      /* 设置 */
      'set.service.title': '服务设置',
      'set.listen': '监听地址',
      'set.listen.hint': '只监听本机时填写 127.0.0.1:端口',
      'set.syncHours': '模型同步间隔（小时）',
      'set.syncHours.hint': '填 0 表示不自动同步',
      'set.language': '语言',
      'set.language.system': '跟随系统',
      'set.language.zh': '简体中文',
      'set.language.en': 'English',
      'set.language.hint': '修改后立即生效，托盘菜单同步切换',
      'set.cors': '允许浏览器跨域调用（CORS）',
      'set.sanitize': '自动规避上游内容审核的固定模板句',
      'set.thinking': 'hy3 系列强制使用最高档思考',
      'set.autostart': '打开程序时自动启动服务',
      'set.launch': '开机自动启动（登录系统后静默运行在托盘）',
      'set.upstream.title': '上游配置',
      'set.upstream.hint': '启用后其模型才会出现在 /v1/models 中',
      'set.provider.enabledFmt': '已启用 · {n} 个账号',
      'set.provider.disabledFmt': '未启用 · {n} 个账号',
      'set.proxyNeeded': '代理，如 http://127.0.0.1:7890',
      'set.proxyOptional': '代理（留空跟随系统）',
      'set.enable': '启用',
      'set.key.title': 'API Key',
      'set.key.hint': '留空表示不校验；设置后客户端必须携带',
      'set.key.empty': '尚未设置 API Key，任何人都可以访问本代理',
      'set.key.newPlaceholder': '粘贴自定义密钥，或点右侧生成',
      'btn.add': '添加',
      'btn.genKey': '随机生成',
      'set.paths.title': '文件位置',
      'set.paths.data': '数据目录',
      'set.paths.config': '配置文件',
      'set.paths.accounts': '凭据目录',
      'set.paths.log': '日志文件',
      'set.paths.version': '程序版本',
      'btn.save': '保存设置',
      'save.saving': '正在保存…',
      'save.done': '已保存',
      'ver.fmt': 'v{v}（{p}）',

      /* 关于 */
      'about.tagline': '把 WorkBuddy / CodeBuddy 背后的模型封装成 OpenAI 兼容的本地网关',
      'about.version': '当前版本',
      'about.platform': '运行平台',
      'about.author': '作者',
      'about.copyright': '版权所有',
      'btn.openRepo': '访问 GitHub 仓库',
      'about.licenseHint': '本项目基于开源协议发布，请遵守上游服务的使用条款',
      'about.update.title': '软件更新',
      'about.update.hint': '检查 GitHub Releases 上的新版本',
      'upd.status.fmt': '当前版本 v{cur}　·　最新版本 v{latest}',
      'upd.status.curOnly': '当前版本 v{cur}',
      'btn.checkUpdate': '检查更新',
      'btn.checking': '检查中…',
      'btn.doUpdate': '下载并安装',

      /* 提示与确认 */
      'toast.copied': '已复制到剪贴板',
      'toast.copyFailedFmt': '复制失败：{err}',
      'toast.backendNotReady': '后端尚未就绪，请稍后重试',
      'toast.stateFailedFmt': '读取状态失败：{err}',
      'toast.loginStartFailedFmt': '发起登录失败：{err}',
      'toast.serviceStarted': '服务已启动',
      'toast.serviceStopped': '服务已停止',
      'toast.opFailedFmt': '操作失败：{err}',
      'toast.syncing': '正在同步…',
      'toast.synced': '模型已同步',
      'toast.syncFailedFmt': '同步失败：{err}',
      'toast.windowReset': '窗口已按当前屏幕重新居中',
      'toast.windowResetFailedFmt': '重置窗口失败：{err}',
      'toast.browserFailedFmt': '打开浏览器失败：{err}',
      'toast.keyEmpty': '请先填写密钥',
      'toast.keyAdded': '已添加密钥',
      'toast.addFailedFmt': '添加失败：{err}',
      'toast.keyDeleted': '已删除密钥',
      'toast.accountDeleted': '已删除账号',
      'toast.deleteFailedFmt': '删除失败：{err}',
      'toast.newKeyFmt': '新密钥已生成：{key}',
      'toast.keyGeneratedFmt': '已生成：{key}',
      'toast.genFailedFmt': '生成失败：{err}',
      'toast.langChanged': '语言已切换',
      'toast.updateAvailable': '发现新版本',
      'toast.updateFailedFmt': '更新失败：{err}',
      'confirm.deleteAccountFmt': '确定删除账号「{name}」吗？',
      'confirm.quit': '确定退出 RapidProxy 吗？退出后代理服务将停止。',
      'confirm.regenKey': '重新生成会替换当前密钥，已配置的软件需要同步更新。继续吗？',
    },

    /* ------------------------------ English ------------------------------ */
    en: {
      /* Shell & navigation */
      'brand.subtitle': 'OpenAI-compatible gateway',
      'nav.overview': 'Overview',
      'nav.accounts': 'Accounts',
      'nav.models': 'Models',
      'nav.settings': 'Settings',
      'nav.about': 'About',
      'btn.quit': 'Quit',
      'btn.resetWindow': 'Reset Window',
      'btn.resetWindow.tip': 'Shrink and center the window when it goes off-screen',
      'btn.hide': 'Minimize to Tray',
      'btn.start': 'Start Service',
      'btn.stop': 'Stop Service',
      'status.running': 'Running',
      'status.stopped': 'Stopped',
      'status.serviceDown': 'Service not running',

      /* Overview */
      'ov.access.title': 'Access Information',
      'ov.access.hint': 'Fill the two items below into any app that supports a custom API',
      'btn.copy': 'Copy',
      'btn.show': 'Show',
      'btn.hideKey': 'Hide',
      'key.regen': 'Regenerate',
      'key.gen': 'Generate Key',
      'key.none': 'Not set (any key is accepted)',
      'ov.requireKey.yes': 'Clients must send this key in the Authorization header',
      'ov.requireKey.no': 'No key is required right now — handy for local debugging',
      'label.models': 'Model List',
      'label.sample': 'Example',
      'btn.copySample': 'Copy Example',
      'ov.status.title': 'Service Status',
      'ov.dt.running': 'Status',
      'ov.dt.listen': 'Listen Address',
      'ov.dt.models': 'Available Models',
      'ov.dt.accounts': 'Logged-in Accounts',
      'ov.dt.sync': 'Model Sync Interval',
      'btn.syncNow': 'Sync Models Now',
      'btn.openDataDir': 'Open Data Directory',
      'ov.providers.title': 'Upstreams',
      'ov.logs.title': 'Logs',
      'btn.clear': 'Clear',
      'tag.enabled': 'Enabled',
      'tag.disabled': 'Disabled',
      'tag.logged': 'Logged in · {n} account(s)',
      'tag.notLogged': 'Not signed in',
      'tag.models': '{n} models',
      'count.unit': '{n}',
      'ov.notLoggedIn': 'Not signed in',
      'sync.every': 'Every {n} h',
      'sync.off': 'Off',

      /* Accounts */
      'acc.login.title': 'Sign In',
      'acc.login.hint': 'Sign in to expose the platform\'s models through the OpenAI API',
      'btn.loginFmt': 'Sign in to {name}',
      'acc.loginDisabled': '(disabled — enable it in Settings after signing in)',
      'login.waiting': 'Waiting for sign-in',
      'login.pending': 'Waiting for authorization in your browser',
      'login.success': 'Signed in',
      'login.failed': 'Sign-in failed',
      'login.browserTip': 'The official sign-in page (Google / GitHub / WeChat QR) has been opened in your default browser. Once you finish authorizing there, this app detects it automatically and saves the credentials. If the browser did not open, copy the link below and visit it manually.',
      'btn.copyLink': 'Copy Link',
      'btn.reopenLogin': 'Reopen Sign-in Page',
      'btn.cancel': 'Cancel',
      'login.expiresFmt': 'Link valid until {time}',
      'acc.list.title': 'Signed-in Accounts',
      'acc.countFmt': '{n} account(s)',
      'acc.empty': 'No accounts yet — click a button above to sign in',
      'acc.uidUnknown': 'unknown',
      'acc.signedAtFmt': 'Signed in {time}',
      'acc.expiresUnknown': 'Expiry unknown',
      'btn.delete': 'Delete',

      /* Models */
      'models.title': 'Available Models',
      'models.search': 'Search models…',
      'btn.sync': 'Sync Now',
      'th.id': 'Model ID',
      'th.name': 'Name',
      'th.provider': 'Provider',
      'th.context': 'Context',
      'th.maxOut': 'Max Output',
      'th.images': 'Images',
      'models.noMatch': 'No matching models',
      'models.imgYes': 'Yes',

      /* Settings */
      'set.service.title': 'Service',
      'set.listen': 'Listen Address',
      'set.listen.hint': 'Use 127.0.0.1:port to listen on this machine only',
      'set.syncHours': 'Model sync interval (hours)',
      'set.syncHours.hint': 'Set 0 to disable automatic syncing',
      'set.language': 'Language',
      'set.language.system': 'Follow System',
      'set.language.zh': '简体中文',
      'set.language.en': 'English',
      'set.language.hint': 'Applies immediately, including the tray menu',
      'set.cors': 'Allow cross-origin browser calls (CORS)',
      'set.sanitize': 'Rewrite canned phrases flagged by upstream moderation',
      'set.thinking': 'Force maximum reasoning effort for hy3 models',
      'set.autostart': 'Start the service automatically when the app opens',
      'set.launch': 'Launch at login (runs silently in the tray)',
      'set.upstream.title': 'Upstreams',
      'set.upstream.hint': 'Only enabled providers appear in /v1/models',
      'set.provider.enabledFmt': 'Enabled · {n} account(s)',
      'set.provider.disabledFmt': 'Disabled · {n} account(s)',
      'set.proxyNeeded': 'Proxy, e.g. http://127.0.0.1:7890',
      'set.proxyOptional': 'Proxy (leave empty to follow system)',
      'set.enable': 'Enabled',
      'set.key.title': 'API Key',
      'set.key.hint': 'Empty means no verification; when set, clients must send it',
      'set.key.empty': 'No API key set — anyone can access this proxy',
      'set.key.newPlaceholder': 'Paste a custom key, or generate one on the right',
      'btn.add': 'Add',
      'btn.genKey': 'Generate',
      'set.paths.title': 'File Locations',
      'set.paths.data': 'Data directory',
      'set.paths.config': 'Config file',
      'set.paths.accounts': 'Credentials directory',
      'set.paths.log': 'Log file',
      'set.paths.version': 'App version',
      'btn.save': 'Save Settings',
      'save.saving': 'Saving…',
      'save.done': 'Saved',
      'ver.fmt': 'v{v} ({p})',

      /* About */
      'about.tagline': 'Wraps the models behind WorkBuddy / CodeBuddy as a local OpenAI-compatible gateway',
      'about.version': 'Version',
      'about.platform': 'Platform',
      'about.author': 'Author',
      'about.copyright': 'Copyright',
      'btn.openRepo': 'GitHub Repository',
      'about.licenseHint': 'This project is open source; please follow the upstream services\' terms of use',
      'about.update.title': 'Software Update',
      'about.update.hint': 'Check GitHub Releases for new versions',
      'upd.status.fmt': 'Current v{cur} · Latest v{latest}',
      'upd.status.curOnly': 'Current version v{cur}',
      'btn.checkUpdate': 'Check for Updates',
      'btn.checking': 'Checking…',
      'btn.doUpdate': 'Download & Install',

      /* Toasts & confirms */
      'toast.copied': 'Copied to clipboard',
      'toast.copyFailedFmt': 'Copy failed: {err}',
      'toast.backendNotReady': 'The backend is not ready yet — please try again shortly',
      'toast.stateFailedFmt': 'Failed to load state: {err}',
      'toast.loginStartFailedFmt': 'Failed to start sign-in: {err}',
      'toast.serviceStarted': 'Service started',
      'toast.serviceStopped': 'Service stopped',
      'toast.opFailedFmt': 'Operation failed: {err}',
      'toast.syncing': 'Syncing…',
      'toast.synced': 'Models synced',
      'toast.syncFailedFmt': 'Sync failed: {err}',
      'toast.windowReset': 'Window re-centered on the current screen',
      'toast.windowResetFailedFmt': 'Failed to reset window: {err}',
      'toast.browserFailedFmt': 'Failed to open the browser: {err}',
      'toast.keyEmpty': 'Enter a key first',
      'toast.keyAdded': 'Key added',
      'toast.addFailedFmt': 'Failed to add: {err}',
      'toast.keyDeleted': 'Key removed',
      'toast.accountDeleted': 'Account removed',
      'toast.deleteFailedFmt': 'Delete failed: {err}',
      'toast.newKeyFmt': 'New key generated: {key}',
      'toast.keyGeneratedFmt': 'Generated: {key}',
      'toast.genFailedFmt': 'Failed to generate: {err}',
      'toast.langChanged': 'Language changed',
      'toast.updateAvailable': 'Update available',
      'toast.updateFailedFmt': 'Update failed: {err}',
      'confirm.deleteAccountFmt': 'Delete account "{name}"?',
      'confirm.quit': 'Quit RapidProxy? The proxy service will stop.',
      'confirm.regenKey': 'Regenerating replaces the current key. Apps configured with it must be updated. Continue?',
    },
  };

  var pref = 'system';
  var lang = effective(pref); // 加载时即按系统语言解析，英文系统首屏不再闪中文

  function detectSystem() {
    var nav = ((navigator.language || navigator.userLanguage || 'zh') + '').toLowerCase();
    return nav.indexOf('zh') === 0 ? 'zh' : 'en';
  }

  function effective(p) {
    return p === 'zh' || p === 'en' ? p : detectSystem();
  }

  function setPref(p) {
    pref = p === 'zh' || p === 'en' ? p : 'system';
    lang = effective(pref);
    try { localStorage.setItem('rapidproxy.langPref', pref); } catch (e) { /* 忽略 */ }
    return lang;
  }

  function t(key, params) {
    var s = (DICT[lang] && DICT[lang][key]) || (DICT.zh && DICT.zh[key]) || key;
    if (params) {
      for (var k in params) {
        if (Object.prototype.hasOwnProperty.call(params, k)) {
          s = s.split('{' + k + '}').join(String(params[k]));
        }
      }
    }
    return s;
  }

  function apply(root) {
    root = root || document;
    document.documentElement.lang = lang === 'zh' ? 'zh-CN' : 'en';
    root.querySelectorAll('[data-i18n]').forEach(function (n) {
      n.textContent = t(n.dataset.i18n);
    });
    root.querySelectorAll('[data-i18n-ph]').forEach(function (n) {
      n.placeholder = t(n.dataset.i18nPh);
    });
    root.querySelectorAll('[data-i18n-title]').forEach(function (n) {
      n.title = t(n.dataset.i18nTitle);
    });
  }

  /* 恢复上次会话的语言偏好，避免启动瞬间闪现默认语言。 */
  try {
    var cached = localStorage.getItem('rapidproxy.langPref');
    if (cached) setPref(cached);
  } catch (e) { /* 忽略 */ }

  window.I18N = {
    t: t,
    apply: apply,
    setPref: setPref,
  };
  Object.defineProperty(window.I18N, 'pref', { get: function () { return pref; } });
  Object.defineProperty(window.I18N, 'lang', { get: function () { return lang; } });
})();
