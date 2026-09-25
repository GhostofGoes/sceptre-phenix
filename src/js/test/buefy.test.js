// installBuefy registers only the Buefy components the UI uses. A b-* tag with
// no registered component renders as an unknown element in production builds
// (Vue only warns in development), so check every tag in the source.
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { createApp } from 'vue';
import { test, expect } from 'vitest';

import { installBuefy } from '@/utils/buefy.js';

const srcDir = join(import.meta.dirname, '..', 'src');

function vueFiles(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) =>
    e.isDirectory()
      ? vueFiles(join(dir, e.name))
      : e.name.endsWith('.vue')
        ? [join(dir, e.name)]
        : [],
  );
}

const pascal = (tag) =>
  tag.replace(/(?:^|-)([a-z])/g, (_, c) => c.toUpperCase());

test('every b-* tag is registered globally or imported by its view', () => {
  const app = createApp({});
  installBuefy(app);
  const global = new Set(Object.keys(app._context.components));

  const missing = [];
  for (const file of vueFiles(srcDir)) {
    const source = readFileSync(file, 'utf8');
    const tags = new Set(
      [...source.matchAll(/<(b-[a-z-]+)/g)].map((m) => pascal(m[1])),
    );
    for (const name of tags) {
      const local = new RegExp(
        `import\\s*{[^}]*\\b${name}\\b[^}]*}\\s*from\\s*'buefy'`,
      );
      if (!global.has(name) && !local.test(source)) {
        missing.push(`${file.slice(srcDir.length + 1)}: ${name}`);
      }
    }
  }

  expect(missing).toEqual([]);
});
