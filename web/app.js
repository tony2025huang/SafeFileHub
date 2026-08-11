// SafeFileHub's browser upload client intentionally has no framework dependency.
// This file is valid JavaScript as well as TypeScript so Node's built-in test
// runner can exercise it when a frontend package/toolchain is not installed.

export const DEFAULT_PER_FILE_CONCURRENCY = 4;

function joinPath(directory, relative) {
  const base = directory === '/' ? '' : directory.replace(/\/+$/, '');
  const pieces = relative.replace(/^\/+/, '').split('/').filter(Boolean);
  return `${base}/${pieces.join('/')}` || '/';
}

export function encodeLogicalPath(path) {
  return path.split('/').map(segment => encodeURIComponent(segment)).join('/');
}

export function createUploadBatch(files, api, options) {
  const settings = {
    rootID: options.rootID,
    directory: options.directory || '/',
    concurrency: options.concurrency || DEFAULT_PER_FILE_CONCURRENCY,
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

  async function run(item) {
    if (item.status === 'cancelled') return;
    item.status = 'uploading';
    item.error = '';
    try {
      if (!item.uploadID) {
        const session = await api.create({ rootID: settings.rootID, path: item.path, size: item.file.size });
        item.uploadID = session.uploadID;
        item.chunkSize = session.chunkSize;
        item.offset = session.offset;
      } else {
        item.offset = await api.offset(item.uploadID);
      }
      item.progress = item.file.size === 0 ? 1 : item.offset / item.file.size;
      while (item.offset < item.file.size) {
        if (item.status !== 'uploading') return;
        const end = Math.min(item.offset + item.chunkSize, item.file.size);
        item.controller = new AbortController();
        await api.patch(item.uploadID, item.offset, item.file.slice(item.offset, end), item.controller.signal);
        item.controller = null;
        item.offset = end;
        item.progress = item.offset / item.file.size;
      }
      if (item.status !== 'uploading') return;
      await api.complete(item.uploadID);
      item.status = 'completed';
      item.progress = 1;
    } catch (error) {
      item.controller = null;
      if (item.status === 'paused' || item.status === 'cancelled') return;
      item.status = 'failed';
      item.error = error instanceof Error ? error.message : String(error);
    }
  }

  async function start() {
    const queue = items.filter(item => item.status === 'queued');
    let next = 0;
    async function worker() {
      while (next < queue.length) await run(queue[next++]);
    }
    await Promise.all(Array.from({ length: Math.min(settings.concurrency, queue.length) }, worker));
    return summary();
  }

  function pause(item) {
    if (item.status === 'uploading') {
      item.status = 'paused';
      item.controller?.abort();
    }
  }
  async function resume(item) {
    if (item.status !== 'paused') return;
    await run(item);
  }
  async function cancel(item) {
    if (item.status === 'completed' || item.status === 'cancelled') return;
    item.status = 'cancelled';
    item.controller?.abort();
    if (item.uploadID) await api.cancel(item.uploadID);
  }
  async function retry(item) {
    if (item.status !== 'failed') return;
    await run(item);
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
    if (!response.ok) throw new Error(`upload request failed (${response.status})`);
    return response;
  }
  return {
    async create(input) {
      const response = await checked(await fetchImpl('/api/uploads', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(input), credentials: 'same-origin',
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

function mountUI() {
  const form = document.querySelector('#upload-form');
  const input = document.querySelector('#files');
  const directory = document.querySelector('#directory');
  const root = document.querySelector('#root-id');
  const list = document.querySelector('#uploads');
  const result = document.querySelector('#summary');
  if (!form || !input || !directory || !root || !list || !result) return;
  form.addEventListener('submit', async event => {
    event.preventDefault();
    const batch = createUploadBatch(input.files, uploadAPI(), { rootID: Number(root.value), directory: directory.value || '/' });
    const render = () => { list.replaceChildren(...batch.items.map(item => {
      const row = document.createElement('li');
      row.textContent = `${item.path} — ${item.status} (${Math.round(item.progress * 100)}%)`;
      for (const [label, action] of [['Pause', () => batch.pause(item)], ['Cancel', () => batch.cancel(item)], ['Retry', () => batch.retry(item)]]) {
        const button = document.createElement('button'); button.type = 'button'; button.textContent = label;
        button.onclick = async () => { await action(); render(); }; row.append(' ', button);
      }
      return row;
    })); };
    render();
    const summary = await batch.start();
    render();
    result.textContent = `${summary.completed}/${summary.total} completed; ${summary.failed} failed; ${summary.cancelled} cancelled`;
  });
}

if (typeof document !== 'undefined') mountUI();
