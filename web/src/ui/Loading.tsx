/** Says that data is on its way. */
export function Loading() {
  return (
    <p className="loading" role="status">
      <span className="spinner-border spinner-border-sm" aria-hidden="true" /> Loading…
    </p>
  );
}
