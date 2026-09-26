import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('@/utils/notify.js', () => ({ openNotification: vi.fn() }));
vi.mock('@/store.js', () => ({ usePhenixStore: vi.fn() }));

import { openNotification } from '@/utils/notify.js';
import { errorHTML, errorSteps, showError } from '@/utils/errorNotif.js';

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

describe('errorSteps', () => {
  it('splits a nested multierror chain into its steps', () => {
    const text =
      'failed to execute Scorch run 0 for experiment nera: 1 error occurred:\n\t* running Scorch for experiment nera: 1 error occurred:\n\t* external user component cc failed: 1 error occurred:\n\t* waiting for command to complete: signal: killed\n\n';

    expect(errorSteps(text)).toEqual([
      'failed to execute Scorch run 0 for experiment nera',
      'running Scorch for experiment nera',
      'external user component cc failed',
      'waiting for command to complete: signal: killed',
    ]);
  });

  it('handles a chain whose newlines were already flattened', () => {
    expect(
      errorSteps('run failed: 2 errors occurred: * first * second: killed'),
    ).toEqual(['run failed', 'first', 'second: killed']);
  });

  it('leaves a plain error whole', () => {
    expect(errorSteps('experiment not found')).toEqual([
      'experiment not found',
    ]);
  });
});

describe('errorHTML', () => {
  it('titles the outermost error and bolds the innermost cause', () => {
    const html = errorHTML(
      'a: 1 error occurred:\n\t* b: 1 error occurred:\n\t* c',
    );

    expect(html).toContain('<b>Error:</b> a</span>');
    expect(html).toContain(
      '<span class="error-step">b</span><span class="error-step"><b>c</b></span>',
    );
  });

  it('adds a separate cause after the message steps', () => {
    expect(errorHTML('bad request', 'line one\nline <two>')).toContain(
      '<b>line one<br>line &lt;two&gt;</b></span>',
    );
  });
});
