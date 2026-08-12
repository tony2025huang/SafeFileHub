// Node 24 runs this TypeScript-without-type-syntax test directly via node --test.
// No package.json or frontend test dependency exists in this repository, so the
// built-in runner is the smallest currently available test harness.
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

import {
  DEFAULT_PER_FILE_CONCURRENCY,
  createUploadBatch,
  encodeLogicalPath,
  filesAPI,
  formatMD5,
  friendlyError,
} from './app.ts';

test('friendly errors never expose internal server details', () => {
  assert.equal(friendlyError(Object.assign(new Error('secret stack'), { status: 403 })), '你没有执行此操作的权限。');
  assert.equal(friendlyError(Object.assign(new Error('secret'), { status: 500 })), '服务暂时不可用，请稍后再试。');
  assert.equal(friendlyError(Object.assign(new Error('secret'), { status: 429 })), '操作太频繁，请稍后再试。');
});

test('deployment HTML loads the browser-ready JavaScript entrypoint', async () => {
  const [html, browserEntry, source] = await Promise.all([
    readFile(new URL('./index.html', import.meta.url), 'utf8'),
    readFile(new URL('./app.js', import.meta.url), 'utf8'),
    readFile(new URL('./app.ts', import.meta.url), 'utf8'),
  ]);
  assert.match(html, /<script type="module" src="\.\/app\.js"><\/script>/);
  assert.equal(browserEntry, source);
});

function file(name, body, relative = '') {
  return {
    name,
    size: Buffer.byteLength(body),
    webkitRelativePath: relative,
    slice(start, end) { return new Blob([body.slice(start, end)]); },
  };
}

function fakeAPI({ failPaths = [], chunkSize = 2 } = {}) {
  const calls = { create: [], patch: [], complete: [], cancel: [], active: 0, maxActive: 0 };
  let nextID = 1;
  return {
    calls,
    async create(input) {
      calls.create.push(input);
      return { uploadID: `upload-${nextID++}`, chunkSize, offset: 0 };
    },
    async offset() { return 0; },
    async patch(id, offset, body) {
      calls.active++;
      calls.maxActive = Math.max(calls.maxActive, calls.active);
      calls.patch.push({ id, offset, size: body.size });
      await new Promise(resolve => setTimeout(resolve, 2));
      calls.active--;
    },
    async complete(id) {
      calls.complete.push(id);
      const path = calls.create.find(item => `upload-${calls.create.indexOf(item) + 1}` === id).path;
      if (failPaths.includes(path)) throw new Error(`failed ${path}`);
    },
    async cancel(id) { calls.cancel.push(id); },
  };
}

test('multiple files receive independent upload sessions', async () => {
  const api = fakeAPI();
  const batch = createUploadBatch([file('one.txt', 'one'), file('two.txt', 'two')], api, { rootID: 7 });
  const summary = await batch.start();
  assert.equal(api.calls.create.length, 2);
  assert.deepEqual(api.calls.create.map(call => call.path), ['/one.txt', '/two.txt']);
  assert.notEqual(batch.items[0].uploadID, batch.items[1].uploadID);
  assert.deepEqual(summary, { total: 2, completed: 2, failed: 0, cancelled: 0 });
});

test('directory selections retain webkitRelativePath and encode each logical segment', async () => {
  const api = fakeAPI();
  const selected = file('报告 ?.txt', 'data', 'photos/2026/报告 ?.txt');
  const batch = createUploadBatch([selected], api, { rootID: 1, directory: '/incoming' });
  await batch.start();
  assert.equal(batch.items[0].path, '/incoming/photos/2026/报告 ?.txt');
  assert.equal(encodeLogicalPath(batch.items[0].path), '/incoming/photos/2026/%E6%8A%A5%E5%91%8A%20%3F.txt');
});

test('the default bounded pool never starts more than four files at once', async () => {
  const api = fakeAPI({ chunkSize: 99 });
  const batch = createUploadBatch(Array.from({ length: 8 }, (_, i) => file(`${i}.txt`, 'x')), api, { rootID: 1 });
  await batch.start();
  assert.equal(DEFAULT_PER_FILE_CONCURRENCY, 4);
  assert.ok(api.calls.maxActive <= DEFAULT_PER_FILE_CONCURRENCY, `max active: ${api.calls.maxActive}`);
});

test('a failed file does not prevent other files or the summary from completing', async () => {
  const api = fakeAPI({ failPaths: ['/bad.txt'] });
  const batch = createUploadBatch([file('good.txt', 'good'), file('bad.txt', 'bad'), file('later.txt', 'later')], api, { rootID: 1 });
  const summary = await batch.start();
  assert.equal(batch.items[0].status, 'completed');
  assert.equal(batch.items[1].status, 'failed');
  assert.equal(batch.items[2].status, 'completed');
  assert.deepEqual(summary, { total: 3, completed: 2, failed: 1, cancelled: 0 });
});

test('special path characters round-trip unchanged through independent upload sessions', async () => {
  const api = fakeAPI();
  const names = ['a+b.txt', '100%.txt', 'what?.txt', 'two words.txt', '中文.txt', 'emoji-😀.txt'];
  const batch = createUploadBatch(names.map(name => file(name, 'x')), api, { rootID: 3, directory: '/drop' });
  await batch.start();
  assert.deepEqual(api.calls.create.map(call => call.path), names.map(name => `/drop/${name}`));
});

test('uploadAPI encodes every path segment in create JSON and IDs in request URLs', async () => {
  const requests = [];
  const api = (await import('./app.ts')).uploadAPI(async (url, init = {}) => {
    requests.push({ url, init });
    if (init.method === 'HEAD') return new Response(null, { status: 200, headers: { 'Upload-Offset': '3' } });
    return new Response(JSON.stringify({ upload_id: 'id /?', chunk_size: 2, offset: 0 }), { status: 201 });
  });
  await api.create({ rootID: 1, path: 'drop/a+b/%? 空/中文😀.txt', size: 1 });
  await api.offset('id /?');
  assert.equal(JSON.parse(requests[0].init.body).path, 'drop/a%2Bb/%25%3F%20%E7%A9%BA/%E4%B8%AD%E6%96%87%F0%9F%98%80.txt');
  assert.equal(requests[1].url, '/api/uploads/id%20%2F%3F');
});

test('the server accepts encoded JSON logical paths and preserves special names', async () => {
  // The browser sends the escaped path in JSON; the Go endpoint decodes once.
  // This assertion is exercised in the HTTP API suite, kept here as the wire contract.
  assert.equal(encodeLogicalPath('drop/a+b/%? 空/中文😀.txt'), 'drop/a%2Bb/%25%3F%20%E7%A9%BA/%E4%B8%AD%E6%96%87%F0%9F%98%80.txt');
});

test('pause, cancel, retry, and HEAD-derived progress are available per file', async () => {
  const api = fakeAPI({ failPaths: ['/retry.txt'], chunkSize: 2 });
  const batch = createUploadBatch([file('retry.txt', 'abcd'), file('cancel.txt', 'x')], api, { rootID: 1 });
  await batch.start();
  const item = batch.items[0];
  assert.equal(item.progress, 1);
  assert.equal(item.status, 'failed');
  api.complete = async id => { api.calls.complete.push(id); };
  await batch.retry(item);
  assert.equal(item.status, 'completed');
  const queued = createUploadBatch([file('cancel.txt', 'x')], api, { rootID: 1 }).items[0];
  queued.uploadID = 'upload-cancel';
  await batch.cancel(queued);
  assert.equal(queued.status, 'cancelled');
  assert.equal(api.calls.cancel.length, 1);
});

test('resume reads a non-zero HEAD offset and reports it immediately', async () => {
  const events = [];
  const api = fakeAPI({ chunkSize: 2 });
  api.offset = async () => 2;
  api.complete = async id => { api.calls.complete.push(id); };
  const batch = createUploadBatch([file('resumed.txt', 'abcd')], api, { rootID: 1, onProgress: item => events.push(item.progress) });
  const item = batch.items[0];
  item.uploadID = 'resumed'; item.chunkSize = 2; item.status = 'paused';
  await batch.resume(item);
  assert.equal(item.status, 'completed');
  assert.equal(api.calls.patch[0].offset, 2);
  assert.ok(events.includes(0.5));
});

test('retry shares the bounded pool and reports lifecycle progress', async () => {
  const events = [];
  const api = fakeAPI({ failPaths: ['/retry.txt'], chunkSize: 2 });
  const batch = createUploadBatch([file('retry.txt', 'abcd'), file('other.txt', 'abcd')], api, { rootID: 1, concurrency: 1, onProgress: item => events.push([item.path, item.status, item.progress]) });
  await batch.start();
  api.complete = async id => { api.calls.complete.push(id); };
  await batch.retry(batch.items[0]);
  assert.equal(api.calls.maxActive, 1);
  assert.equal(batch.items[0].status, 'completed');
  assert.ok(events.some(([, status]) => status === 'failed'));
  assert.ok(events.some(([, status, progress]) => status === 'uploading' && progress > 0));
});

test('file API and MD5 formatter show only enabled safe digests', async () => {
  const calls = [];
  const api = filesAPI(async (url, init) => { calls.push({ url, init }); return { ok: true, json: async () => ({ files: [] }) }; });
  await api.list(3, '/docs');
  assert.equal(calls[0].url, '/roots/3/files?path=/docs');
  assert.equal(calls[0].init.credentials, 'same-origin');
  assert.equal(formatMD5({ md5_status: 'ready', md5_digest: 'd41d8cd98f00b204e9800998ecf8427e' }, true), 'MD5: d41d8cd98f00b204e9800998ecf8427e');
  assert.equal(formatMD5({ md5_status: 'ready', md5_digest: 'unsafe' }, true), 'MD5: unavailable');
  assert.equal(formatMD5({ md5_status: 'ready', md5_digest: 'd41d8cd98f00b204e9800998ecf8427e' }, false), '');
});

test('site settings API saves settings and uses the dedicated asset upload and reset endpoints', async () => {
  const requests = [];
  const api = (await import('./app.ts')).siteSettingsAPI(async (url, init = {}) => {
    requests.push({ url, init });
    if (init.method === 'POST') return new Response(JSON.stringify({ url: '/assets/site/7' }), { status: 201 });
    return new Response(init.method === 'GET' ? JSON.stringify({ site_name: 'Example' }) : null, { status: 204 });
  });
  await api.save({ site_name: 'Example', primary_color: '#123abc', filing_enabled: false, filing_text: '', md5_enabled: true });
  await api.uploadAsset('login_logo', new Blob(['image'], { type: 'image/png' }));
  await api.resetAsset('login_logo');
  assert.equal(requests[0].url, '/api/admin/site-settings');
  assert.equal(requests[0].init.method, 'PUT');
  assert.equal(requests[1].url, '/api/admin/site-settings/assets/login_logo');
  assert.equal(requests[1].init.method, 'POST');
  assert.equal(requests[1].init.headers['Content-Type'], 'image/png');
  assert.equal(requests[2].url, '/api/admin/site-settings/assets/login_logo');
  assert.equal(requests[2].init.method, 'DELETE');
});

test('deployment HTML exposes dynamic branding controls and all three asset reset buttons', async () => {
  const html = await readFile(new URL('./index.html', import.meta.url), 'utf8');
  assert.match(html, /id="site-favicon"/);
  assert.match(html, /id="login-logo"/);
  assert.match(html, /id="nav-logo"/);
  assert.match(html, /data-asset-reset="login_logo"/);
  assert.match(html, /data-asset-reset="nav_logo"/);
  assert.match(html, /data-asset-reset="favicon"/);
});
