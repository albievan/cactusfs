import { useCallback } from 'react';

import { api, unwrap, type AuditPage, type AuditRecord } from '../api/client';
import { navigate, routeHref } from '../router';
import { formatTime } from '../ui/format';
import { useAction, useLoad } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { ProblemNotice } from '../ui/ProblemNotice';

const PAGE_SIZE = 50;
// The most the API returns at once; used to walk to the end of the trail.
const WALK_SIZE = 1000;

function fetchPage(after: number, limit: number): Promise<AuditPage> {
  return unwrap(api.GET('/audit', { params: { query: { after, limit } } }));
}

function auditHref(after: number): string {
  return after === 0 ? routeHref(['audit']) : routeHref(['audit'], { after: String(after) });
}

interface AuditScreenProps {
  /** Show the records whose sequence number is greater than this. */
  after: number;
}

/**
 * The audit trail, in the order it was written. The API pages forward from a
 * sequence number, so the place in the trail is that number and lives in the
 * address.
 */
export function AuditScreen({ after }: AuditScreenProps) {
  const load = useCallback(() => fetchPage(after, PAGE_SIZE), [after]);
  const page = useLoad(load);
  const action = useAction();
  const nextAfter = page.data?.next_after;

  // The API has no way to ask for the last page, so walk to it: follow
  // next_after in the largest steps allowed, then show the tail of the last
  // step.
  const newest = () => {
    void action.run(async () => {
      let cursor = after;
      let last = await fetchPage(cursor, WALK_SIZE);
      while (last.next_after !== undefined) {
        cursor = last.next_after;
        last = await fetchPage(cursor, WALK_SIZE);
      }
      const before = last.records.at(-PAGE_SIZE - 1);
      navigate(auditHref(before === undefined ? cursor : before.sequence));
    });
  };

  return (
    <>
      <h1>Audit log</h1>
      <ProblemNotice error={action.error ?? page.error} />

      <section className="card card-body panel" aria-labelledby="audit-records">
        <h2 id="audit-records">{after === 0 ? 'Records from the beginning' : `Records after number ${after}`}</h2>
        <div className="pager">
          <button
            type="button"
            className="btn btn-outline-secondary"
            disabled={after === 0}
            onClick={() => {
              navigate(auditHref(0));
            }}
          >
            Oldest
          </button>
          <button
            type="button"
            className="btn btn-outline-secondary"
            disabled={after === 0}
            onClick={() => {
              navigate(auditHref(Math.max(0, after - PAGE_SIZE)));
            }}
          >
            Older
          </button>
          <button
            type="button"
            className="btn btn-outline-secondary"
            disabled={nextAfter === undefined}
            onClick={() => {
              if (nextAfter !== undefined) {
                navigate(auditHref(nextAfter));
              }
            }}
          >
            Newer
          </button>
          <button type="button" className="btn btn-outline-secondary" disabled={action.busy} onClick={newest}>
            Newest
          </button>
          <button type="button" className="btn btn-outline-secondary" onClick={page.reload}>
            <i className="bi bi-arrow-clockwise" aria-hidden="true" /> Refresh
          </button>
        </div>
        {page.loading ? <Loading /> : null}
        {page.data !== undefined && page.data.records.length === 0 ? <p>There are no records here.</p> : null}
        {page.data !== undefined && page.data.records.length > 0 ? (
          <table className="table table-hover align-middle grid">
            <thead>
              <tr>
                <th scope="col">Number</th>
                <th scope="col">Time</th>
                <th scope="col">Actor</th>
                <th scope="col">Action</th>
                <th scope="col">Resource</th>
                <th scope="col">Outcome</th>
                <th scope="col">Details</th>
              </tr>
            </thead>
            <tbody>
              {page.data.records.map((record) => (
                <tr key={record.sequence}>
                  <td data-label="Number">{record.sequence}</td>
                  <td data-label="Time">{formatTime(record.time)}</td>
                  <td data-label="Actor">{record.actor || '-'}</td>
                  <td data-label="Action">{record.action}</td>
                  <td data-label="Resource">{record.resource || '-'}</td>
                  <td data-label="Outcome">{record.outcome}</td>
                  <td data-label="Details">
                    <RecordDetails record={record} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : null}
      </section>
    </>
  );
}

function RecordDetails({ record }: { record: AuditRecord }) {
  return (
    <details>
      <summary aria-label={`Details of record ${record.sequence}`}>Show</summary>
      <dl className="facts">
        {record.access_key_id === undefined ? null : (
          <>
            <dt>Access key ID</dt>
            <dd>
              <code>{record.access_key_id}</code>
            </dd>
          </>
        )}
        {record.source_ip === undefined ? null : (
          <>
            <dt>Source address</dt>
            <dd>{record.source_ip}</dd>
          </>
        )}
        {record.request_id === undefined ? null : (
          <>
            <dt>Request ID</dt>
            <dd>
              <code>{record.request_id}</code>
            </dd>
          </>
        )}
        {Object.entries(record.details ?? {}).map(([name, value]) => (
          <div key={name} className="facts-pair">
            <dt>{name}</dt>
            <dd>{value}</dd>
          </div>
        ))}
        <dt>Hash</dt>
        <dd>
          <code>{record.hash}</code>
        </dd>
        <dt>Previous hash</dt>
        <dd>
          <code>{record.previous_hash}</code>
        </dd>
      </dl>
    </details>
  );
}
