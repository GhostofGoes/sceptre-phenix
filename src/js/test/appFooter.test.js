// @vitest-environment happy-dom
import { afterEach, expect, test, vi } from 'vitest';

import AppFooter from '@/components/AppFooter.vue';
import { DOCS_URL } from '@/utils/docs.js';
import { mountPage } from './helpers/mount.js';

afterEach(() => {
  vi.unstubAllGlobals();
});

test('links to the documentation once, with a Help link that says it opens a new tab', async () => {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve({ json: () => Promise.resolve({}) })),
  );
  const wrapper = await mountPage(AppFooter);

  const links = wrapper.findAll('a');
  expect(links.map((a) => a.attributes('href'))).toEqual([DOCS_URL]);
  expect(links[0].text().replace(/\s+/g, ' ')).toBe('Help (opens in new tab)');
  expect(links[0].attributes('target')).toBe('_blank');
  expect(links[0].attributes('rel')).toContain('noopener');
  expect(wrapper.find('nav').attributes('aria-label')).toBe('Help');
});
