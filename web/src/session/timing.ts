// When the console asks the API how the session stands. This file holds no
// React and asks nothing: what it says is only when to ask. Whether a session
// is locked or has ended is for the API to say, never for a clock in here.

/** How often a locked session is asked after, and how soon a session whose timeout has passed is asked after again. */
export const SESSION_POLL_MS = 15_000;

/**
 * How long after the timeout the API names the console asks. The API says
 * the moment to the second and counts it more finely: asked on the second,
 * it may answer that the timeout has not come.
 */
export const SESSION_ASK_AFTER_MS = 1000;

/**
 * The longest a timer is set for: 24 days. setTimeout keeps its delay in 32
 * bits and fires at once when it is given more than about 24.8 days. A
 * timer that is cut short asks early, is told the same timeout, and is set
 * again.
 */
export const SESSION_TIMER_MAX_MS = 24 * 24 * 60 * 60 * 1000;

/**
 * In how many milliseconds a session that is not locked is asked after,
 * given the timeout the API named for it and the time it is now - or null
 * when there is nothing to wait for: the API named no timeout, as a node of
 * the release before sessions could lock does, or one that cannot be read.
 *
 * A timeout that has passed already - the clock here is ahead of the API's,
 * or the API was asked a moment early - is not asked after at once, which
 * would be asked without end, but as often as a locked session is.
 */
export function askDelay(expiresAt: string | null, now: number): number | null {
  if (expiresAt === null) {
    return null;
  }
  const at = Date.parse(expiresAt);
  if (Number.isNaN(at)) {
    return null;
  }
  if (at <= now) {
    return SESSION_POLL_MS;
  }
  return Math.min(at - now + SESSION_ASK_AFTER_MS, SESSION_TIMER_MAX_MS);
}
