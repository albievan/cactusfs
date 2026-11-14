import { ApiError, type Problem } from '../api/client';

interface ProblemNoticeProps {
  /** What went wrong: an ApiError, another error, or null when nothing did. */
  error: unknown;
  /**
   * What the console says of a problem it knows, before the API's own words:
   * for example what a code means where the request was made. Undefined for
   * a problem it says nothing more of. It words the API's answer and decides
   * nothing.
   */
  explain?: (problem: Problem) => string | undefined;
}

/**
 * Says why a request was not carried out, in the words of the API: the title,
 * the detail and the request ID of its problem+json answer. The region is
 * always in the page, so that a screen reader announces an error that appears.
 */
export function ProblemNotice({ error, explain }: ProblemNoticeProps) {
  return (
    <div className="notice-region" aria-live="assertive">
      {error === null || error === undefined ? null : <ProblemBody error={error} explain={explain} />}
    </div>
  );
}

function ProblemBody({ error, explain }: ProblemNoticeProps) {
  if (error instanceof ApiError) {
    const { title, detail, request_id: requestId } = error.problem;
    const words = explain?.(error.problem);
    const said = detail === undefined || detail === '' ? undefined : detail;
    return (
      <div className="alert alert-danger notice" role="alert">
        {words === undefined ? (
          <>
            <p className="notice-title">{title}</p>
            {said === undefined ? null : <p>{said}</p>}
          </>
        ) : (
          <>
            <p className="notice-title">{words}</p>
            <p>
              The server said: {title}
              {said === undefined ? '' : `: ${said}`}
            </p>
          </>
        )}
        {requestId === undefined || requestId === '' ? null : (
          <p className="notice-meta">
            Request ID: <code>{requestId}</code>
          </p>
        )}
      </div>
    );
  }
  const message = error instanceof Error ? error.message : String(error);
  return (
    <div className="alert alert-danger notice" role="alert">
      <p className="notice-title">{message}</p>
    </div>
  );
}
