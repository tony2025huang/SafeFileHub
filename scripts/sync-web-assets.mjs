import { copyFile } from 'node:fs/promises';
const root = new URL('..', import.meta.url);
await copyFile(new URL('web/index.html', root), new URL('internal/httpapi/assets/index.html', root));
await copyFile(new URL('web/app.ts', root), new URL('web/app.js', root));
await copyFile(new URL('web/app.js', root), new URL('internal/httpapi/assets/app.js', root));
