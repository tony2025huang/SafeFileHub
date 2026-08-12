// SafeFileHub's browser upload client intentionally has no framework dependency.
// This file is valid JavaScript as well as TypeScript so Node's built-in test
// runner can exercise it when a frontend package/toolchain is not installed.

export const DEFAULT_PER_FILE_CONCURRENCY = 4;
export const SESSION_CHECK_INTERVAL_MS = 5 * 60 * 1000;

// Authentication is cookie based. Keep all browser-side handling here so an
// expired/revoked cookie cannot leave a stale transfer UI behind. The server
// remains the authority: this guard never stores or reads the HttpOnly cookie.
export function createSessionGuard({
  fetchImpl = fetch,
  navigate = path => { if (typeof location !== 'undefined') location.assign(path); },
  storage = typeof sessionStorage !== 'undefined' ? sessionStorage : undefined,
  win = typeof window !== 'undefined' ? window : undefined,
  doc = typeof document !== 'undefined' ? document : undefined,
  setIntervalImpl = setInterval,
  clearIntervalImpl = clearInterval,
  intervalMs = SESSION_CHECK_INTERVAL_MS,
} = {}) {
  let invalidated = false;
  let checkInFlight = null;
  let timer = null;
  const listeners = [];
  const clearClientState = () => {
    // These are legacy/application-owned names only; never erase unrelated
    // storage belonging to other same-origin applications.
    for (const key of ['safefilehub_session', 'safefilehub_auth', 'safefilehub_token']) {
      try { storage?.removeItem(key); } catch (_) {}
    }
  };
  const authError = () => Object.assign(new Error('authentication required'), { status: 401 });
  const goLogin = () => {
    if (!win || win.location?.pathname !== '/login') navigate('/login');
  };
  const invalidate = () => {
    if (invalidated) return;
    invalidated = true;
    if (timer !== null) { clearIntervalImpl(timer); timer = null; }
    clearClientState();
    // Clear a still-valid cookie after the server reports the authenticated
    // session is invalid. Do not wait for this best-effort request before leaving.
    void fetchImpl('/logout', { method: 'POST', credentials: 'same-origin' }).catch(() => {});
    goLogin();
  };
  const inspect = response => {
    // A 403 means the authenticated user lacks access to this resource (for
    // example, a non-admin probing the settings endpoint). It must stop that
    // action without destroying an otherwise valid session. A 401 is the
    // server's unambiguous signal that the session has expired or was revoked.
    if (response?.status === 401) invalidate();
    return response;
  };
  const request = async (input, init) => {
    if (invalidated) throw authError();
    const response = inspect(await fetchImpl(input, init));
    if (invalidated) throw authError();
    return response;
  };
  const check = () => {
    if (invalidated) return Promise.resolve(false);
    if (checkInFlight) return checkInFlight;
    checkInFlight = (async () => {
      try {
        const response = inspect(await fetchImpl('/session', { credentials: 'same-origin', cache: 'no-store' }));
        return Boolean(response?.ok) && !invalidated;
      } catch (_) {
        // A network failure is not proof of expiry. Preserve local work and
        // retry on the next interval/focus rather than logging the user out.
        return false;
      } finally { checkInFlight = null; }
    })();
    return checkInFlight;
  };
  const start = async () => {
    const active = await check();
    if (!active || invalidated) return false;
    if (timer === null) timer = setIntervalImpl(() => { void check(); }, intervalMs);
    const onFocus = () => { void check(); };
    const onVisibility = () => { if (!doc?.hidden) void check(); };
    const onPageShow = () => { void check(); };
    for (const [target, name, listener] of [[win, 'focus', onFocus], [win, 'pageshow', onPageShow], [doc, 'visibilitychange', onVisibility]]) {
      target?.addEventListener?.(name, listener);
      listeners.push([target, name, listener]);
    }
    return true;
  };
  const stop = () => {
    if (timer !== null) { clearIntervalImpl(timer); timer = null; }
    for (const [target, name, listener] of listeners.splice(0)) target?.removeEventListener?.(name, listener);
  };
  const logout = async () => {
    const response = await fetchImpl('/logout', { method: 'POST', credentials: 'same-origin' });
    if (!response.ok) { const error = new Error('logout failed'); error.status = response.status; throw error; }
    invalidate();
  };
  return { fetch: request, check, start, stop, logout, invalidate, get invalidated() { return invalidated; } };
}

export function friendlyError(error) {
  const status = Number(error?.status || 0);
  if (status === 401) return '登录已过期，请重新登录。';
  if (status === 403) return '你没有执行此操作的权限。';
  if (status === 400) return '请求无法处理，请检查文件名或路径后重试。';
  if (status === 413) return '内容超过大小限制。';
  if (status === 409) return '文件状态已变化，请刷新后重试。';
  if (status === 429) return '操作太频繁，请稍后再试。';
  if (status >= 500) return '服务暂时不可用，请稍后再试。';
  if (error?.name === 'TypeError' || error?.name === 'AbortError') return '网络连接失败，请检查网络后重试。';
  return '操作失败，请稍后再试。';
}

function serverReason(error) {
  const reason = String(error?.reason || '').toLowerCase();
  if (reason.includes('destination exists')) return '目标已存在（409），请改名后重试。';
  if (reason.includes('parent directory not found')) return '父目录不存在，请刷新后重试。';
  if (reason.includes('offset conflict')) return '上传偏移已变化，请重试。';
  return '';
}

export function selectView(name, doc = document, win = typeof window !== 'undefined' ? window : undefined) {
  const view = name === 'settings' ? 'settings' : 'files';
  for (const section of doc.querySelectorAll('[data-view]')) section.hidden = section.dataset.view !== view;
  for (const link of doc.querySelectorAll('[data-view-link]')) {
    const active = link.dataset.viewLink === view;
    link.setAttribute('aria-current', active ? 'page' : 'false');
  }
  if (win && win.location.hash !== `#${view}`) win.history.replaceState(null, '', `#${view}`);
  return view;
}

function joinPath(directory, relative) {
  const base = directory === '/' ? '' : directory.replace(/\/+$/, '');
  const pieces = relative.replace(/^\/+/, '').split('/').filter(Boolean);
  return `${base}/${pieces.join('/')}` || '/';
}

export function encodeLogicalPath(path) {
  return path.split('/').map(segment => encodeURIComponent(segment)).join('/');
}

// File mutations use relative logical paths; the list endpoint alone reserves
// '/' as its root-directory query value. Keep the canonical UI path separate
// from this wire representation so paths are never double-decoded.
export function mutationPath(path) {
  return path === '/' ? '' : path.replace(/^\/+/, '').replace(/\/+$/, '');
}

export function createUploadBatch(files, api, options) {
  const settings = {
    rootID: options.rootID,
    directory: options.directory || '/',
    concurrency: options.concurrency || DEFAULT_PER_FILE_CONCURRENCY,
    onProgress: options.onProgress || (() => {}),
  };
  if (!Number.isInteger(settings.rootID) || settings.rootID <= 0) throw new Error('a positive rootID is required');
  if (!Number.isInteger(settings.concurrency) || settings.concurrency < 1) throw new Error('concurrency must be positive');

  const items = Array.from(files, file => ({
    file,
    // webkitRelativePath is deliberately used verbatim when present: browsers
    // use it to retain every selected directory segment.
    path: joinPath(settings.directory, file.webkitRelativePath || file.name),
    uploadID: '',
    chunkSize: 0,
    offset: 0,
    progress: 0,
    status: 'queued',
    error: '',
    controller: null,
  }));

  function notify(item) { settings.onProgress(item); }
  function setStatus(item, status) { item.status = status; notify(item); }
  function reportProgress(item) { item.progress = item.file.size === 0 ? 1 : item.offset / item.file.size; notify(item); }

  async function run(item) {
    if (item.status === 'cancelled') return;
    setStatus(item, 'uploading');
    item.error = '';
    try {
      if (!item.uploadID) {
        const session = await api.create({ rootID: settings.rootID, path: mutationPath(item.path), size: item.file.size });
        item.uploadID = session.uploadID;
        item.chunkSize = session.chunkSize;
        item.offset = session.offset;
      } else {
        item.offset = await api.offset(item.uploadID);
      }
      reportProgress(item);
      while (item.offset < item.file.size) {
        if (item.status !== 'uploading') return;
        const end = Math.min(item.offset + item.chunkSize, item.file.size);
        item.controller = new AbortController();
        await api.patch(item.uploadID, item.offset, item.file.slice(item.offset, end), item.controller.signal);
        item.controller = null;
        item.offset = end;
        reportProgress(item);
      }
      if (item.status !== 'uploading') return;
      await api.complete(item.uploadID);
      item.progress = 1;
      setStatus(item, 'completed');
    } catch (error) {
      item.controller = null;
      if (item.status === 'paused' || item.status === 'cancelled') return;
      item.error = serverReason(error) || friendlyError(error);
      setStatus(item, 'failed');
    }
  }

  const queue = [];
  let active = 0;
  let draining = false;
  function enqueue(item) {
    return new Promise(resolve => { queue.push({ item, resolve }); drain(); });
  }
  function drain() {
    if (draining) return;
    draining = true;
    const work = async () => {
      while (queue.length || active) {
        while (active < settings.concurrency && queue.length) {
          const job = queue.shift(); active++;
          run(job.item).finally(() => { active--; job.resolve(); drain(); });
        }
        if (active) await new Promise(resolve => setTimeout(resolve, 0));
      }
      draining = false;
    };
    void work();
  }
  async function start() {
    await Promise.all(items.filter(item => item.status === 'queued').map(enqueue));
    return summary();
  }

  function pause(item) {
    if (item.status === 'uploading') {
      setStatus(item, 'paused');
      item.controller?.abort();
    }
  }
  async function resume(item) {
    if (item.status !== 'paused') return;
    await enqueue(item);
  }
  async function cancel(item) {
    if (item.status === 'completed' || item.status === 'cancelled') return;
    setStatus(item, 'cancelled');
    item.controller?.abort();
    if (item.uploadID) await api.cancel(item.uploadID);
  }
  async function retry(item) {
    if (item.status !== 'failed') return;
    await enqueue(item);
  }
  function summary() {
    return items.reduce((result, item) => {
      result.total++;
      if (item.status === 'completed') result.completed++;
      if (item.status === 'failed') result.failed++;
      if (item.status === 'cancelled') result.cancelled++;
      return result;
    }, { total: 0, completed: 0, failed: 0, cancelled: 0 });
  }
  return { items, start, pause, resume, cancel, retry, summary };
}

export function uploadAPI(fetchImpl = fetch) {
  async function checked(response) {
    if (!response.ok) { const error = new Error('request failed'); error.status = response.status; throw error; }
    return response;
  }
  return {
    async create(input) {
      const response = await checked(await fetchImpl('/api/uploads', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ ...input, path: encodeLogicalPath(mutationPath(input.path)) }), credentials: 'same-origin',
      }));
      const value = await response.json();
      return { uploadID: value.upload_id, chunkSize: value.chunk_size, offset: value.offset };
    },
    async offset(id) {
      const response = await checked(await fetchImpl(`/api/uploads/${encodeURIComponent(id)}`, { method: 'HEAD', credentials: 'same-origin' }));
      const offset = Number(response.headers.get('Upload-Offset'));
      if (!Number.isSafeInteger(offset) || offset < 0) throw new Error('server returned an invalid upload offset');
      return offset;
    },
    async patch(id, offset, body, signal) {
      await checked(await fetchImpl(`/api/uploads/${encodeURIComponent(id)}`, {
        method: 'PATCH', headers: { 'Content-Type': 'application/offset+octet-stream', 'Upload-Offset': String(offset) }, body, signal, credentials: 'same-origin',
      }));
    },
    async complete(id) { await checked(await fetchImpl(`/api/uploads/${encodeURIComponent(id)}/complete`, { method: 'POST', credentials: 'same-origin' })); },
    async cancel(id) { await checked(await fetchImpl(`/api/uploads/${encodeURIComponent(id)}`, { method: 'DELETE', credentials: 'same-origin' })); },
  };
}

export function filesAPI(fetchImpl = fetch) {
  async function checked(response) { if (!response.ok) { const error = new Error('request failed'); error.status = response.status; try { error.reason = (await response.text()).slice(0, 160); } catch (_) {} throw error; } return response; }
  return {
    async list(rootID, path = '/') {
      if (!Number.isInteger(rootID) || rootID <= 0) throw new Error('a positive rootID is required');
      return checked(await fetchImpl(`/roots/${rootID}/files?path=${encodeLogicalPath(path)}`, { credentials: 'same-origin' })).then(response => response.json());
    },
    async createDirectory(rootID, path) { await checked(await fetchImpl('/api/directories', { method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify({root_id: rootID, path: encodeLogicalPath(mutationPath(path))}), credentials:'same-origin' })); },
    async createFile(rootID, path, content = '') { await checked(await fetchImpl('/api/files', { method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify({root_id: rootID, path: encodeLogicalPath(mutationPath(path)), content}), credentials:'same-origin' })); },
    async renameFile(fileID, path) { await checked(await fetchImpl(`/api/files/${encodeURIComponent(fileID)}`, { method:'PATCH', headers:{'Content-Type':'application/json'}, body:JSON.stringify({path: encodeLogicalPath(mutationPath(path))}), credentials:'same-origin' })); },
    async deleteFile(fileID) { await checked(await fetchImpl(`/api/files/${encodeURIComponent(fileID)}`, { method: 'DELETE', credentials: 'same-origin' })); },
    async deleteDirectory(directoryID) { await checked(await fetchImpl(`/api/directories/${encodeURIComponent(directoryID)}`, { method: 'DELETE', credentials: 'same-origin' })); },
    async archive(rootID, path) {
      const response = await checked(await fetchImpl(`/api/roots/${rootID}/archives`, { method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({path: encodeLogicalPath(path)}), credentials:'same-origin' }));
      return response.json();
    },
    downloadURL(fileID) { return `/api/files/${encodeURIComponent(fileID)}`; },
  };
}

export function formatFileSize(size) {
  const value = Number(size);
  if (!Number.isFinite(value) || value < 0) return '—';
  if (value < 1024) return `${value} B`;
  const units = ['KB', 'MB', 'GB', 'TB']; let n = value / 1024; let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(n >= 10 ? 0 : 1)} ${units[i]}`;
}

export function formatModified(value) {
  if (!value) return '—';
  const date = new Date(value); return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString('zh-CN');
}

export function formatMD5(file, enabled) {
  if (!enabled || !file) return '';
  if (file.md5_status === 'ready' && /^[a-f0-9]{32}$/.test(file.md5_digest || '')) return `MD5: ${file.md5_digest}`;
  const labels = { pending: 'MD5: pending', computing: 'MD5: computing', failed: 'MD5: unavailable', disabled: 'MD5: disabled' };
  return labels[file.md5_status] || 'MD5: unavailable';
}

export function siteSettingsAPI(fetchImpl = fetch) {
  async function checked(response) {
    if (!response.ok) { const error = new Error('site settings request failed'); error.status = response.status; throw error; }
    return response;
  }
  return {
    async publicSettings() {
      return checked(await fetchImpl('/api/site-settings', { credentials: 'same-origin' })).then(response => response.json());
    },
    async adminSettings() {
      return checked(await fetchImpl('/api/admin/site-settings', { credentials: 'same-origin' })).then(response => response.json());
    },
    async save(settings) {
      await checked(await fetchImpl('/api/admin/site-settings', {
        method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(settings), credentials: 'same-origin',
      }));
    },
    async uploadAsset(kind, file) {
      return checked(await fetchImpl(`/api/admin/site-settings/assets/${encodeURIComponent(kind)}`, {
        method: 'POST', headers: { 'Content-Type': file.type }, body: file, credentials: 'same-origin',
      })).then(response => response.json());
    },
    async resetAsset(kind) {
      await checked(await fetchImpl(`/api/admin/site-settings/assets/${encodeURIComponent(kind)}`, { method: 'DELETE', credentials: 'same-origin' }));
    },
  };
}

export function applySiteSettings(settings, doc = document) {
  const siteName = settings.site_name || 'SafeFileHub';
  doc.title = `${siteName} transfers`;
  for (const element of doc.querySelectorAll('[data-site-name]')) element.textContent = siteName;
  if (/^#[0-9a-f]{6}$/i.test(settings.primary_color || '')) doc.documentElement.style.setProperty('--primary-color', settings.primary_color);

  const setLogo = (selector, url, alt) => {
    const image = doc.querySelector(selector);
    if (!image) return;
    image.alt = alt;
    if (url) { image.src = url; image.hidden = false; } else { image.removeAttribute('src'); image.hidden = true; }
  };
  setLogo('#login-logo', settings.login_logo_url, `${siteName} logo`);
  setLogo('#nav-logo', settings.nav_logo_url, `${siteName} logo`);
  const favicon = doc.querySelector('#site-favicon');
  if (favicon && settings.favicon_url) favicon.href = settings.favicon_url;
  const filing = doc.querySelector('#filing');
  if (filing) { filing.textContent = settings.filing_text || ''; filing.hidden = !settings.filing_enabled || !settings.filing_text; }
}

function setAdminForm(settings, form) {
  form.elements.site_name.value = settings.site_name || '';
  form.elements.primary_color.value = settings.primary_color || '#2563eb';
  form.elements.filing_enabled.checked = Boolean(settings.filing_enabled);
  form.elements.filing_text.value = settings.filing_text || '';
  form.elements.md5_enabled.checked = Boolean(settings.md5_enabled);
}

function mountUI() {
  const form = document.querySelector('#upload-form');
  const input = document.querySelector('#files');
  const directories = document.querySelector('#directories');
  // The product has one system-managed primary root. Keep its API identifier
  // internal; users choose only the current logical directory.
  const root = { value: '1' };
  const directory = { value: '/' };
  const list = document.querySelector('#uploads');
  const result = document.querySelector('#summary');
  const fileList = document.querySelector('#file-list');
  const refreshFiles = document.querySelector('#refresh-files');
  const filesStatus = document.querySelector('#files-status');
  const fileSearch = document.querySelector('#file-search');
  const breadcrumb = document.querySelector('#breadcrumb');
  const fileActions = document.querySelector('#file-actions');
  if (!form || !input || !directories || !list || !result) return;
  const session = createSessionGuard();
  // Begin validation before wiring protected UI actions. API calls remain
  // guarded too, covering requests made while this initial probe is pending.
  const sessionReady = session.start();
  let selectedInput = input;
  let queuedFiles = [];
  let queuedBatch = null;
  let uploadStarted = false;
  const settingsView = document.querySelector('[data-view="settings"]');
  const filesView = document.querySelector('[data-view="files"]');
  const settingsLink = document.querySelector('[data-view-link="settings"]');
  const filesLink = document.querySelector('[data-view-link="files"]');
  const navigate = name => selectView(name);
  for (const link of [settingsLink, filesLink]) link?.addEventListener('click', event => {
    event.preventDefault(); navigate(link.dataset.viewLink);
  });
  navigate(location.hash === '#settings' ? 'settings' : 'files');
  const renderQueue = batch => { list.replaceChildren(...(batch?.items || []).map(item => {
    const row = document.createElement('li'); row.className = 'upload-row';
    const info = document.createElement('div'); info.className = 'upload-info';
    const title = document.createElement('strong'); title.textContent = item.path;
    const details = document.createElement('span'); details.className = 'status'; details.textContent = `${item.file.name} · ${formatFileSize(item.file.size)} · 目标：${item.path}`;
    const progress = document.createElement('progress'); progress.className = 'progress'; progress.max = 1; progress.value = item.progress;
    const state = document.createElement('span'); state.className = 'badge'; state.textContent = ({ queued: '待上传', uploading: '上传中', paused: '已暂停', completed: '已完成', failed: '失败', cancelled: '已取消' })[item.status] || item.status;
    info.append(title, details, progress, state); if (item.error) { const error = document.createElement('span'); error.className = 'error'; error.textContent = item.error; info.append(error); }
    row.append(info);
    if (!uploadStarted && item.status === 'queued') { const remove = document.createElement('button'); remove.type = 'button'; remove.className = 'danger'; remove.textContent = '移除'; remove.onclick = () => { const index = queuedBatch.items.indexOf(item); if (index >= 0) queuedBatch.items.splice(index, 1); queuedFiles.splice(index, 1); renderQueue(queuedBatch); result.textContent = `${queuedBatch.items.length} 项待上传`; }; row.append(remove); }
    for (const [label, action] of [['暂停', () => queuedBatch.pause(item)], ['取消', () => queuedBatch.cancel(item)], ['重试', () => queuedBatch.retry(item)]]) { if (item.status === 'queued' && !uploadStarted && label !== '取消') continue; const button = document.createElement('button'); button.type = 'button'; button.textContent = label; button.onclick = async () => { await action(); renderQueue(queuedBatch); }; row.append(' ', button); }
    return row;
  })); };
  const addSelectedFiles = files => {
    if (uploadStarted) return;
    const seen = new Set(queuedFiles.map(file => `${file.webkitRelativePath || file.name}\u0000${file.size}\u0000${file.lastModified || 0}`));
    for (const file of files) { const key = `${file.webkitRelativePath || file.name}\u0000${file.size}\u0000${file.lastModified || 0}`; if (!seen.has(key)) { queuedFiles.push(file); seen.add(key); } }
    queuedBatch = createUploadBatch(queuedFiles, uploadAPI(session.fetch), { rootID: Number(root.value), directory: directory.value || '/', onProgress: () => renderQueue(queuedBatch) });
    renderQueue(queuedBatch); result.textContent = `${queuedBatch.items.length} 项待上传`; input.value = ''; directories.value = '';
  };
  input.addEventListener('change', () => addSelectedFiles(input.files));
  directories.addEventListener('change', () => addSelectedFiles(directories.files));
  form.addEventListener('submit', async event => {
    event.preventDefault();
    if (!queuedBatch || !queuedBatch.items.length) { result.textContent = '请先选择要上传的文件或目录。'; return; }
    if (uploadStarted) return;
    uploadStarted = true; renderQueue(queuedBatch);
    const summary = await queuedBatch.start(); renderQueue(queuedBatch);
    result.textContent = `上传完成：${summary.completed}/${summary.total}；失败 ${summary.failed}；已取消 ${summary.cancelled}`;
    await renderFiles();
  });

  const api = siteSettingsAPI(session.fetch);
  const fileAPI = filesAPI(session.fetch);
  let publicSettings = { md5_enabled: false };
  let listedFiles = [];
  const renderFiles = async () => {
    if (!fileList || !filesStatus) return;
    filesStatus.textContent = '正在加载文件…';
    try {
      const response = await fileAPI.list(Number(root.value), directory.value || '/');
      listedFiles = Array.isArray(response.files) ? response.files : [];
      const query = (fileSearch?.value || '').trim().toLowerCase();
      const files = listedFiles.filter(file => !query || `${file.name} ${file.path}`.toLowerCase().includes(query));
      files.sort((a,b) => Number(Boolean(b.is_directory)) - Number(Boolean(a.is_directory)) || a.name.localeCompare(b.name));
      fileList.replaceChildren(...files.map(file => {
        const row = document.createElement('li'); row.className = 'file-row';
        const name = document.createElement(file.is_directory ? 'button' : 'strong'); name.textContent = `${file.is_directory ? '📁' : '📄'} ${file.name || file.path}`;
        if (file.is_directory) { name.className = 'link-button'; name.onclick = () => { directory.value = file.path; void renderFiles(); }; }
        const meta = document.createElement('span'); meta.className = 'file-meta';
        meta.textContent = file.is_directory ? '文件夹' : `${formatFileSize(file.size)} · ${formatModified(file.updated_at || file.modified_at)}${formatMD5(file, publicSettings.md5_enabled) ? ` · ${formatMD5(file, publicSettings.md5_enabled)}` : ''}`;
        const actions = document.createElement('span'); actions.className = 'actions';
        if (!file.is_directory && file.id) { const download = document.createElement('a'); download.href = fileAPI.downloadURL(file.id); download.textContent = '下载'; download.className = 'button secondary'; download.setAttribute('download',''); actions.append(download); }
        if (!file.is_directory && file.id) { const rename = document.createElement('button'); rename.type='button'; rename.textContent='重命名'; rename.onclick=async()=>{ const name=prompt('新文件名（仅支持文件）',file.name || ''); if(!name?.trim())return; try { await fileAPI.renameFile(file.id,joinPath(directory.value||'/',name.trim())); filesStatus.textContent='文件已重命名。'; await renderFiles(); } catch(error) { filesStatus.textContent=friendlyError(error); } }; actions.append(rename); }
        if (file.id) { const remove = document.createElement('button'); remove.type='button'; remove.textContent='删除'; remove.className='danger'; remove.onclick = async () => { if (!confirm(`确认删除“${file.name || file.path}”？此操作不可撤销。`)) return; remove.disabled=true; try { if (file.is_directory) await fileAPI.deleteDirectory(file.id); else await fileAPI.deleteFile(file.id); filesStatus.textContent='操作成功。'; await renderFiles(); } catch(error) { remove.disabled=false; filesStatus.textContent=friendlyError(error); } }; actions.append(remove); }
        row.append(name, meta, actions); return row;
      }));
      filesStatus.textContent = files.length ? `${files.length} 项` : (query ? '没有匹配的文件。' : '当前目录为空。');
      const uploadTarget = document.querySelector('#upload-target');
      if (uploadTarget) uploadTarget.textContent = directory.value || '/';
      if (breadcrumb) { breadcrumb.replaceChildren(); const parts = (directory.value || '/').split('/').filter(Boolean); const rootLink = document.createElement('button'); rootLink.className='link-button'; rootLink.textContent='/'; rootLink.onclick=()=>{directory.value='/'; void renderFiles();}; breadcrumb.append(rootLink); let path='/'; parts.forEach(part=>{ path += `${part}/`; const link=document.createElement('button'); link.className='link-button'; link.textContent=` ${part} /`; const target=path.replace(/\/$/,''); link.onclick=()=>{directory.value=target; void renderFiles();}; breadcrumb.append(link); }); }
    } catch (error) { fileList.replaceChildren(); filesStatus.textContent = friendlyError(error); }
  };
  if (refreshFiles) refreshFiles.addEventListener('click', () => { void renderFiles(); });
  const uploadFile = document.querySelector('[data-action="upload-file"]');
  const uploadDir = document.querySelector('[data-action="upload-dir"]');
  if (uploadFile) uploadFile.addEventListener('click', () => { selectedInput = input; input.click(); });
  if (uploadDir) uploadDir.addEventListener('click', () => { selectedInput = directories; directories.click(); });
  if (fileSearch) fileSearch.addEventListener('input', () => { void renderFiles(); });
  if (fileActions) fileActions.addEventListener('click', async event => { const button = event.target.closest('button[data-action]'); if (!button || !['mkdir', 'text', 'archive'].includes(button.dataset.action)) return; if (button.dataset.action === 'archive') { try { const job=await fileAPI.archive(Number(root.value), directory.value || '/'); filesStatus.textContent=`归档任务已创建：${job.id || '处理中'}`; } catch(error) { filesStatus.textContent=friendlyError(error); } return; }
    const name = prompt(button.dataset.action === 'mkdir' ? '新建目录名称' : '新建文本名称'); if (!name?.trim()) return;
    const path = joinPath(directory.value || '/', name.trim());
    try { if (button.dataset.action === 'text') { const content = prompt('文本内容（UTF-8，最大 16 MiB）', ''); if (content === null) return; await fileAPI.createFile(Number(root.value), path, content); filesStatus.textContent = '文本文件已创建并写入内容。'; } else { await fileAPI.createDirectory(Number(root.value), path); filesStatus.textContent = '目录已创建。'; } await renderFiles(); } catch (error) { filesStatus.textContent = friendlyError(error); }
  });
  const admin = document.querySelector('#admin-settings');
  const adminForm = document.querySelector('#admin-settings-form');
  const adminStatus = document.querySelector('#admin-settings-status');
  const showAdminStatus = message => { if (adminStatus) adminStatus.textContent = message; };
  const logout = document.querySelector('#logout');
  if (logout) logout.addEventListener('click', async () => { logout.disabled = true; try { await session.logout(); } catch (error) { logout.disabled = false; showAdminStatus(friendlyError(error)); } });
  const refreshBranding = async () => {
    const settings = await api.publicSettings();
    applySiteSettings(settings);
    publicSettings = settings;
    return settings;
  };
  void (async () => {
    // Do not render protected data merely because the shell was restored from
    // browser cache. The real /session endpoint validates the server session.
    if (!await sessionReady) return;
    await refreshBranding().catch(() => {});
    await renderFiles();
  })();

  if (!admin || !adminForm) return;
  void sessionReady.then(active => {
    if (!active) throw Object.assign(new Error('authentication required'), { status: 401 });
    return api.adminSettings();
  }).then(settings => {
    setAdminForm(settings, adminForm);
    admin.hidden = false;
    if (settingsLink) settingsLink.hidden = false;
  }).catch(error => {
    admin.hidden = true;
    if (settingsLink) settingsLink.hidden = true;
    if (Number(error?.status) === 403) showAdminStatus('仅管理员可使用站点设置。');
    else if (Number(error?.status) !== 401) showAdminStatus(friendlyError(error));
  });
  adminForm.addEventListener('submit', async event => {
    event.preventDefault();
    const fields = adminForm.elements;
    try {
      await api.save({
        site_name: fields.site_name.value,
        primary_color: fields.primary_color.value,
        filing_enabled: fields.filing_enabled.checked,
        filing_text: fields.filing_text.value,
        md5_enabled: fields.md5_enabled.checked,
      });
      await refreshBranding();
      showAdminStatus('Settings saved.');
    } catch (error) { showAdminStatus(friendlyError(error)); }
  });
  for (const upload of admin.querySelectorAll('[data-asset-upload]')) upload.addEventListener('change', async event => {
    const file = event.target.files?.[0];
    if (!file) return;
    try {
      await api.uploadAsset(upload.dataset.assetUpload, file);
      await refreshBranding();
      showAdminStatus('Logo uploaded.');
      event.target.value = '';
    } catch (error) { showAdminStatus(friendlyError(error)); }
  });
  for (const reset of admin.querySelectorAll('[data-asset-reset]')) reset.addEventListener('click', async () => {
    try {
      await api.resetAsset(reset.dataset.assetReset);
      await refreshBranding();
      showAdminStatus('Logo reset.');
    } catch (error) { showAdminStatus(friendlyError(error)); }
  });
}

if (typeof document !== 'undefined') mountUI();
