import { describe, expect, it } from 'vitest';
import { fileText } from '@/utils/fileText.js';

describe('fileText', () => {
  it('indents JSON files', () => {
    expect(fileText('run.JSON', '{"a":[1]}')).toBe(
      '{\n  "a": [\n    1\n  ]\n}',
    );
  });

  it('leaves other files and invalid JSON as they are', () => {
    expect(fileText('out.log', '{"a":1}')).toBe('{"a":1}');
    expect(fileText('bad.json', '{"a":')).toBe('{"a":');
  });

  it('shows nothing for a missing body', () => {
    expect(fileText('x.json', undefined)).toBe('');
  });
});
