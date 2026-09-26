import { describe, expect, it } from 'vitest';
import { inForeground } from '@/utils/foreground.js';

describe('inForeground', () => {
  it('is true for a visible, focused page', () => {
    expect(inForeground({ hidden: false, hasFocus: () => true })).toBe(true);
  });

  it('is false for a hidden tab', () => {
    expect(inForeground({ hidden: true, hasFocus: () => true })).toBe(false);
  });

  it('is false for a visible window without focus', () => {
    expect(inForeground({ hidden: false, hasFocus: () => false })).toBe(false);
  });
});
