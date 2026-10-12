import { api, unwrap } from '../api/client';
import { formatBytes, formatTime } from '../ui/format';
import { useLoad } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { ProblemNotice } from '../ui/ProblemNotice';

function loadCluster() {
  return unwrap(api.GET('/cluster'));
}

function loadMaintenance() {
  return unwrap(api.GET('/cluster/maintenance'));
}

/**
 * The state of the deployment: nodes, erasure sets and their drives, and what
 * scrubbing, healing and collection have done on the node that answered.
 */
export function ClusterScreen() {
  const cluster = useLoad(loadCluster);
  const maintenance = useLoad(loadMaintenance);
  const status = cluster.data;
  const work = maintenance.data;

  return (
    <>
      <h1>Cluster</h1>
      <p>
        <button
          type="button"
          className="btn btn-outline-secondary"
          onClick={() => {
            cluster.reload();
            maintenance.reload();
          }}
          title="Load this again"
        >
          <i className="bi bi-arrow-clockwise" aria-hidden="true" /> <span className="btn-text">Refresh</span>
        </button>
      </p>
      <ProblemNotice error={cluster.error} />

      {cluster.loading ? <Loading /> : null}
      {status === undefined ? null : (
        <>
          <section className="card card-body panel" aria-labelledby="cluster-summary">
            <h2 id="cluster-summary">Deployment</h2>
            <dl className="stats">
              <div className="stat">
                <dt>Drives online</dt>
                <dd>
                  {status.drives_online} of {status.drives_total}
                </dd>
              </div>
              <div className="stat">
                <dt>Node failures tolerated</dt>
                <dd>{status.node_failures_tolerated}</dd>
              </div>
              <div className="stat">
                <dt>Metadata leader</dt>
                <dd>{status.leader === '' ? 'None' : status.leader}</dd>
              </div>
              <div className="stat">
                <dt>Answered by</dt>
                <dd>{status.answered_by}</dd>
              </div>
              <div className="stat stat-wide">
                <dt>Deployment ID</dt>
                <dd>
                  <code>{status.deployment_id}</code>
                </dd>
              </div>
            </dl>
          </section>

          <section className="card card-body panel" aria-labelledby="cluster-nodes">
            <h2 id="cluster-nodes">Nodes</h2>
            <table className="table table-hover align-middle grid">
              <thead>
                <tr>
                  <th scope="col">Node</th>
                  <th scope="col">Roles</th>
                  <th scope="col">Leader</th>
                  <th scope="col">Region</th>
                  <th scope="col">Zone</th>
                  <th scope="col">Rack</th>
                  <th scope="col">Host</th>
                  <th scope="col">Metadata version</th>
                </tr>
              </thead>
              <tbody>
                {status.nodes.map((node) => (
                  <tr key={node.id}>
                    <td data-label="Node">{node.id}</td>
                    <td data-label="Roles">{node.roles.join(', ') || '-'}</td>
                    <td data-label="Leader">{node.leader ? 'Yes' : 'No'}</td>
                    <td data-label="Region">{node.labels.region ?? '-'}</td>
                    <td data-label="Zone">{node.labels.zone ?? '-'}</td>
                    <td data-label="Rack">{node.labels.rack ?? '-'}</td>
                    <td data-label="Host">{node.labels.host ?? '-'}</td>
                    <td data-label="Metadata version">{node.metadata_version ?? '-'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </section>

          <section className="card card-body panel" aria-labelledby="cluster-sets">
            <h2 id="cluster-sets">Erasure sets</h2>
            {status.erasure_sets.length === 0 ? <p>There are no erasure sets.</p> : null}
            {status.erasure_sets.map((set) => (
              <div key={set.index}>
                <h3>Erasure set {set.index}</h3>
                <table className="table table-hover align-middle grid">
                  <thead>
                    <tr>
                      <th scope="col">Drive</th>
                      <th scope="col">Node</th>
                      <th scope="col">State</th>
                    </tr>
                  </thead>
                  <tbody>
                    {set.drives.map((drive) => (
                      <tr key={drive.id}>
                        <td data-label="Drive">
                          <code>{drive.id}</code>
                        </td>
                        <td data-label="Node">{drive.node_id}</td>
                        <td data-label="State">
                          <span className={`state state-${drive.state}`}>{drive.state}</span>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            ))}
          </section>
        </>
      )}

      <section className="card card-body panel" aria-labelledby="cluster-maintenance">
        <h2 id="cluster-maintenance">Maintenance</h2>
        <ProblemNotice error={maintenance.error} />
        {maintenance.loading ? <Loading /> : null}
        {work === undefined ? null : (
          <>
            <p>
              Background work of node {work.node_id} since it started at {formatTime(work.started_at)}.
            </p>
            <h3>Scrubbing</h3>
            <dl className="facts">
              <dt>Passes</dt>
              <dd>{work.scrub.passes}</dd>
              <dt>Last pass</dt>
              <dd>{formatTime(work.scrub.last_pass_at)}</dd>
              <dt>Objects checked in the last pass</dt>
              <dd>{work.scrub.objects}</dd>
              <dt>Bytes checked</dt>
              <dd>{formatBytes(work.scrub.bytes)}</dd>
              <dt>Damaged objects</dt>
              <dd>{work.scrub.damaged}</dd>
              <dt>Shards rebuilt</dt>
              <dd>{work.scrub.shards_rebuilt}</dd>
              <dt>Unrecoverable objects</dt>
              <dd>{work.scrub.unrecoverable}</dd>
            </dl>
            <h3>Healing</h3>
            <dl className="facts">
              <dt>Objects found damaged</dt>
              <dd>{work.heal.objects}</dd>
              <dt>Shards rebuilt</dt>
              <dd>{work.heal.shards_rebuilt}</dd>
              <dt>Unrecoverable objects</dt>
              <dd>{work.heal.unrecoverable}</dd>
            </dl>
            <h3>Collection</h3>
            <dl className="facts">
              <dt>Passes</dt>
              <dd>{work.collection.passes}</dd>
              <dt>Last pass</dt>
              <dd>{formatTime(work.collection.last_pass_at)}</dd>
              <dt>Shards looked at in the last pass</dt>
              <dd>{work.collection.shards}</dd>
              <dt>Shards removed</dt>
              <dd>{work.collection.removed}</dd>
            </dl>
          </>
        )}
      </section>
    </>
  );
}
