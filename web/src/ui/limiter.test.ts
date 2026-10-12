import { describe, expect, it } from 'vitest';

import { limiter } from './limiter';

/** A task that ends when the test says so. */
function held() {
  let end: (value: string) => void = () => undefined;
  let begun = false;
  const done = new Promise<string>((resolve) => {
    end = resolve;
  });
  return {
    task: () => {
      begun = true;
      return done;
    },
    end,
    begun: () => begun,
  };
}

/** Lets what has been resolved so far run on. */
async function settle() {
  for (let i = 0; i < 10; i += 1) {
    await Promise.resolve();
  }
}

describe('limiter', () => {
  it('runs at most so many tasks at a time, in the order they came', async () => {
    const run = limiter(2);
    const tasks = [held(), held(), held(), held()];
    const results = tasks.map((t) => run(t.task));
    await settle();
    expect(tasks.map((t) => t.begun())).toEqual([true, true, false, false]);

    tasks[1]?.end('second');
    await settle();
    expect(tasks.map((t) => t.begun())).toEqual([true, true, true, false]);
    expect(await results[1]).toBe('second');

    tasks[0]?.end('first');
    tasks[2]?.end('third');
    tasks[3]?.end('fourth');
    expect(await Promise.all(results)).toEqual(['first', 'second', 'third', 'fourth']);
  });

  it('gives the place of a task that failed to the next', async () => {
    const run = limiter(1);
    const failed = run(() => Promise.reject(new Error('no')));
    const after = held();
    const result = run(after.task);
    await expect(failed).rejects.toThrow('no');
    await settle();
    expect(after.begun()).toBe(true);
    after.end('done');
    expect(await result).toBe('done');
  });

  it('does not begin a task that was given up while it waited', async () => {
    const run = limiter(1);
    const first = held();
    const running = run(first.task);
    const given = new AbortController();
    const second = held();
    const waited = run(second.task, given.signal);
    const third = held();
    const last = run(third.task);
    given.abort(new Error('given up'));
    first.end('first');
    expect(await running).toBe('first');
    await expect(waited).rejects.toThrow('given up');
    await settle();
    expect(second.begun()).toBe(false);
    // Its turn went to the one after it.
    expect(third.begun()).toBe(true);
    third.end('third');
    expect(await last).toBe('third');
  });
});
