import { ApiError } from '../api/client';

interface ProblemNoticeProps {
  /** What went wrong: an ApiError, another error, or null when nothing did. */
  error: unknown;
}

/**
 * Says why a request was not carried out, in the words of the API: the title,
 * the detail and the request ID of its problem+json answer. The region is
 * always in the page, so that a screen reader announces an error that appears.
 */
export function ProblemNotice({ error }: ProblemNoticeProps) {
  return (
    <div className="notice-region" aria-live="assertive">
      {error === null || error === undefined ? null : <ProblemBody error={error} />}
    </div>
  );
}

function ProblemBody({ error }: { error: unknown }) {
  if (error instanceof ApiError) {
    const { title, detail, request_id: requestId } = error.problem;
    return (
      <div className="alert alert-danger notice" role="alert">
        <p className="notice-title">{title}</p>
        {detail === undefined || detail === '' ? null : <p>{detail}</p>}
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
