import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('@/utils/notify.js', () => ({ openNotification: vi.fn() }));
vi.mock('@/store.js', () => ({ usePhenixStore: vi.fn() }));

import { openNotification } from '@/utils/notify.js';
import { showError } from '@/utils/errorNotif.js';

describe('showError', () => {
  beforeEach(() => openNotification.mockClear());

  it('opens an error notification with an icon', () => {
    showError('SCORCH run 0 failed');

    expect(openNotification).toHaveBeenCalledWith(
      expect.objectContaining({ type: 'is-danger', hasIcon: true }),
    );
  });

  it('escapes server text so it cannot inject markup', () => {
    showError('<img src=x onerror=alert(1)>', 'line one\nline <two>');

    const { message } = openNotification.mock.calls[0][0];
    expect(message).not.toContain('<img');
    expect(message).toContain('&lt;img src=x onerror=alert(1)&gt;');
    expect(message).toContain('line one<br>line &lt;two&gt;');
  });
});
