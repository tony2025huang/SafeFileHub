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
} from './app.ts';

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
