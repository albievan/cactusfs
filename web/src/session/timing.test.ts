import { describe, expect, it } from 'vitest';

import { askDelay, SESSION_ASK_AFTER_MS, SESSION_POLL_MS, SESSION_TIMER_MAX_MS } from './timing';

const NOW = Date.parse('2026-10-13T12:00:00Z');

describe('when the console asks how the session stands', () => {
  const cases: { name: string; expiresAt: string | null; delay: number | null }[] = [
    { name: 'a moment after the timeout the API named', expiresAt: '2026-10-13T12:30:00Z', delay: 30 * 60_000 + SESSION_ASK_AFTER_MS },
    { name: 'with fractions of a second read', expiresAt: '2026-10-13T12:00:00.250Z', delay: 250 + SESSION_ASK_AFTER_MS },
    { name: 'never, when the API named no timeout', expiresAt: null, delay: null },
    { name: 'never, when the timeout cannot be read', expiresAt: 'soon', delay: null },
    { name: 'never, when the timeout is empty', expiresAt: '', delay: null },
    { name: 'not at once when the timeout is this very moment', expiresAt: '2026-10-13T12:00:00Z', delay: SESSION_POLL_MS },
    { name: 'not at once when the timeout has passed', expiresAt: '2026-10-13T11:00:00Z', delay: SESSION_POLL_MS },
    { name: 'not at once when the timeout passed long ago', expiresAt: '1999-01-01T00:00:00Z', delay: SESSION_POLL_MS },
    { name: 'no later than a timer can be set for', expiresAt: '2026-12-31T00:00:00Z', delay: SESSION_TIMER_MAX_MS },
    { name: 'no later than that for a timeout years away', expiresAt: '2099-01-01T00:00:00Z', delay: SESSION_TIMER_MAX_MS },
  ];

  for (const { name, expiresAt, delay } of cases) {
    it(name, () => {
      expect(askDelay(expiresAt, NOW)).toBe(delay);
    });
  }

  it('never names a delay that setTimeout would take for none, nor one that is not in the future', () => {
    // setTimeout keeps its delay in a signed 32-bit integer.
    expect(SESSION_TIMER_MAX_MS).toBeLessThan(2 ** 31);
    for (let hours = -48; hours <= 24 * 60; hours += 7) {
      const delay = askDelay(new Date(NOW + hours * 3_600_000).toISOString(), NOW);
      expect(delay).not.toBeNull();
      expect(delay).toBeGreaterThan(0);
      expect(delay).toBeLessThanOrEqual(SESSION_TIMER_MAX_MS);
    }
  });
});
