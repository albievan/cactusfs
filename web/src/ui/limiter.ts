/** Runs tasks, at most so many at a time; see limiter. */
export type Limiter = <T>(task: () => Promise<T>, signal?: AbortSignal) => Promise<T>;

/**
 * Returns a function that runs the tasks it is given at most `limit` at a
 * time, in the order they came; the others wait their turn. A task whose
 * signal was aborted while it waited is not begun: its promise rejects with
 * the reason of the signal, and its turn goes to the next.
 */
export function limiter(limit: number): Limiter {
  let running = 0;
  const waiting: (() => void)[] = [];

  // The place of a task that has ended goes straight to the one that waited
  // longest, so that nobody who comes in between can take it.
  const release = () => {
    const next = waiting.shift();
    if (next === undefined) {
      running -= 1;
    } else {
      next();
    }
  };

  return async <T>(task: () => Promise<T>, signal?: AbortSignal): Promise<T> => {
    if (running < limit) {
      running += 1;
    } else {
      await new Promise<void>((turn) => {
        waiting.push(turn);
      });
    }
    try {
      signal?.throwIfAborted();
      return await task();
    } finally {
      release();
    }
  };
}
