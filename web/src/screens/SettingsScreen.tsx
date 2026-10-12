import { useState, type ReactNode, type ToggleEvent } from 'react';

import {
  api,
  unwrap,
  type DriveSettings,
  type NodeSettings,
  type Setting,
  type Settings,
  type SettingsCertificate,
  type SettingsFilesystem,
  type SettingsMismatch,
  type SettingsVolume,
} from '../api/client';
import { describeBuild } from '../Layout';
import { formatBytes, formatTime } from '../ui/format';
import { useLoad } from '../ui/hooks';
import { Loading } from '../ui/Loading';
import { ProblemNotice } from '../ui/ProblemNotice';

/** What was read, and when: the certificates are weighed against that moment. */
interface Read {
  settings: Settings;
  /** When the answer arrived, in milliseconds since the epoch. */
  at: number;
}

async function loadSettings(signal: AbortSignal): Promise<Read> {
  const settings = await unwrap(api.GET('/settings', { signal }));
  return { settings, at: Date.now() };
}

// Why a node did not answer, in words. The API may one day name a reason
// this console does not know: that one is shown as it came.
const REASONS = new Map<string, string>([
  ['unreachable', 'could not be reached'],
  ['timeout', 'did not answer in time'],
  ['unsupported', 'runs an older release that cannot say'],
  ['failed', 'could not read its settings'],
]);

function reasonInWords(reason: string | undefined): string {
  if (reason === undefined || reason === '') {
    return 'no reason was given';
  }
  return REASONS.get(reason) ?? reason;
}

// The kinds of setting whose value is shown. Every other kind - `secret`,
// `secret-file`, and any kind a node of a newer release names that this
// console does not know - is shown as "set" or "not set" and nothing more.
const SHOWN_KINDS = new Set(['plain', 'path', 'url']);

/** How long before its end a certificate is said to expire soon. */
const EXPIRES_SOON_MS = 14 * 24 * 60 * 60 * 1000;

/** A drive ID is long: its beginning is shown, all of it is in the hint. */
function shortID(id: string): string {
  return id.length > 12 ? `${id.slice(0, 8)}…` : id;
}

function filesystemText(filesystem: SettingsFilesystem | undefined): string {
  if (filesystem === undefined) {
    return '-';
  }
  const parts = [filesystem.type, filesystem.device, filesystem.mount_point].filter((part) => part !== '');
  return parts.length === 0 ? '-' : parts.join(' · ');
}

/**
 * Whether a setting is listed: its name, its variable or its flag holds what
 * was typed, whatever the case, and - when only what was changed is asked
 * for - its value did not come from the default. This chooses what is on
 * screen and nothing else.
 */
function isListed(setting: Setting, sought: string, overriddenOnly: boolean): boolean {
  if (overriddenOnly && setting.source === 'default') {
    return false;
  }
  if (sought === '') {
    return true;
  }
  const texts = [setting.name, setting.env, setting.flag, setting.flag === undefined ? undefined : `--${setting.flag}`];
  return texts.some((text) => text !== undefined && text.toLowerCase().includes(sought));
}

interface SettingGroup {
  name: string;
  settings: Setting[];
}

/** The settings under their headings, headings and settings in the order the API gave them. */
function groupSettings(settings: Setting[]): SettingGroup[] {
  const groups: SettingGroup[] = [];
  const byName = new Map<string, SettingGroup>();
  for (const setting of settings) {
    const name = setting.group === undefined || setting.group === '' ? 'Other' : setting.group;
    let group = byName.get(name);
    if (group === undefined) {
      group = { name, settings: [] };
      byName.set(name, group);
      groups.push(group);
    }
    group.settings.push(setting);
  }
  return groups;
}

/**
 * How every node of the deployment is configured: what holds for the
 * deployment as a whole, the settings in which the nodes differ and should
 * not, and for every node its settings, its drives and the directories they
 * are in, and the certificates it serves. The view is live - the nodes are
 * asked as the screen is loaded - and nothing on it can be changed here.
 */
export function SettingsScreen() {
  const loaded = useLoad(loadSettings);
  const [sought, setSought] = useState('');
  const [overriddenOnly, setOverriddenOnly] = useState(false);
  // The cards somebody opened or closed. A card nobody touched is open when
  // it is that of the node that answered.
  const [toggled, setToggled] = useState<ReadonlyMap<string, boolean>>(new Map());

  const read = loaded.data;
  const settings = read?.settings;
  const nodes = settings?.nodes ?? [];
  const answering = nodes.filter((node) => node.answered);
  const isOpen = (node: NodeSettings) => toggled.get(node.node_id) ?? node.node_id === settings?.answered_by;
  const allOpen = answering.length > 0 && answering.every(isOpen);

  const setOpen = (nodeID: string, open: boolean) => {
    setToggled((previous) => {
      if (previous.get(nodeID) === open) {
        return previous;
      }
      return new Map(previous).set(nodeID, open);
    });
  };

  const setAllOpen = (open: boolean) => {
    setToggled(new Map(answering.map((node) => [node.node_id, open])));
  };

  return (
    <>
      <h1>Settings</h1>
      <p>
        <button type="button" className="btn btn-outline-secondary" onClick={loaded.reload} title="Ask the nodes for their settings again">
          <i className="bi bi-arrow-clockwise" aria-hidden="true" /> <span className="btn-text">Reload</span>
        </button>
      </p>
      <ProblemNotice error={loaded.error} />

      {loaded.loading ? <Loading /> : null}
      {settings === undefined || read === undefined ? null : (
        <>
          <ClusterPanel settings={settings} />
          {settings.mismatches.length === 0 ? null : <MismatchesPanel mismatches={settings.mismatches} />}

          <h2 id="settings-nodes">Nodes</h2>
          <div className="toolbar settings-toolbar">
            <label className="field settings-filter">
              <span>Filter settings</span>
              <input
                className="form-control form-control-sm"
                type="search"
                name="filter"
                placeholder="Name, variable or flag"
                maxLength={128}
                autoCapitalize="none"
                autoComplete="off"
                spellCheck={false}
                value={sought}
                onChange={(event) => {
                  setSought(event.target.value);
                }}
              />
            </label>
            <label className="check">
              <input
                className="form-check-input"
                type="checkbox"
                checked={overriddenOnly}
                onChange={(event) => {
                  setOverriddenOnly(event.target.checked);
                }}
              />
              <span>Show only overridden</span>
            </label>
            <button
              type="button"
              className="btn btn-outline-secondary"
              disabled={answering.length === 0}
              onClick={() => {
                setAllOpen(!allOpen);
              }}
              title={allOpen ? 'Close the card of every node' : 'Open the card of every node'}
            >
              <i className={allOpen ? 'bi bi-arrows-collapse' : 'bi bi-arrows-expand'} aria-hidden="true" />{' '}
              <span className="btn-text">{allOpen ? 'Collapse all' : 'Expand all'}</span>
            </button>
          </div>

          {nodes.length === 0 ? <p>No node is recorded.</p> : null}
          {nodes.map((node) => (
            <NodeCard
              key={node.node_id}
              node={node}
              open={isOpen(node)}
              onOpen={setOpen}
              sought={sought.trim().toLowerCase()}
              overriddenOnly={overriddenOnly}
              now={read.at}
            />
          ))}
        </>
      )}
    </>
  );
}

/** What holds for the deployment as a whole, and how each erasure set is coded. */
function ClusterPanel({ settings }: { settings: Settings }) {
  const { cluster } = settings;
  return (
    <section className="card card-body panel" aria-labelledby="settings-cluster">
      <h2 id="settings-cluster">Cluster</h2>
      <dl className="stats">
        <div className="stat">
          <dt>Metadata schema level</dt>
          <dd>{cluster.metadata_level}</dd>
        </div>
        <div className="stat">
          <dt>Metadata leader</dt>
          <dd>{cluster.leader === undefined || cluster.leader === '' ? 'None' : cluster.leader}</dd>
        </div>
        <div className="stat">
          <dt>Nodes</dt>
          <dd>{cluster.nodes}</dd>
        </div>
        <div className="stat">
          <dt>Voters</dt>
          <dd>{cluster.voters}</dd>
        </div>
        <div className="stat">
          <dt>Answered by</dt>
          <dd>{settings.answered_by}</dd>
        </div>
        <div className="stat stat-wide">
          <dt>Deployment ID</dt>
          <dd>{cluster.deployment_id === '' ? '-' : <code>{cluster.deployment_id}</code>}</dd>
        </div>
      </dl>

      <h3>Erasure sets</h3>
      {cluster.erasure_sets.length === 0 ? (
        <p>There are no erasure sets.</p>
      ) : (
        <div className="table-responsive grid-scroll">
          <table className="table table-hover align-middle grid grid-keep" aria-label="Erasure sets">
            <thead>
              <tr>
                <th scope="col" className="grid-number">
                  Set
                </th>
                <th scope="col" className="grid-number">
                  Drives
                </th>
                <th scope="col" className="grid-number">
                  Data
                </th>
                <th scope="col" className="grid-number">
                  Parity
                </th>
                <th scope="col" className="grid-number">
                  Read quorum
                </th>
                <th scope="col" className="grid-number">
                  Write quorum
                </th>
              </tr>
            </thead>
            <tbody>
              {cluster.erasure_sets.map((set) => (
                <tr key={set.index}>
                  <td className="grid-number">{set.index}</td>
                  <td className="grid-number">{set.drives}</td>
                  <td className="grid-number">{set.data}</td>
                  <td className="grid-number">{set.parity}</td>
                  <td className="grid-number">{set.read_quorum}</td>
                  <td className="grid-number">{set.write_quorum}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

/** The settings that should be the same on every node and are not, as the API found them. */
function MismatchesPanel({ mismatches }: { mismatches: SettingsMismatch[] }) {
  return (
    <section className="card card-body panel" aria-labelledby="settings-mismatches">
      <h2 id="settings-mismatches">Mismatches</h2>
      <p className="alert alert-warning settings-warning">
        <i className="bi bi-exclamation-triangle-fill" aria-hidden="true" /> <strong>Warning:</strong> These settings must be the same on
        every node and are not:
      </p>
      <div className="table-responsive grid-scroll">
        <table className="table table-hover align-middle grid grid-keep" aria-label="Settings that differ between nodes">
          <thead>
            <tr>
              <th scope="col">Setting</th>
              <th scope="col">Node</th>
              <th scope="col">Value</th>
            </tr>
          </thead>
          <tbody>
            {mismatches.flatMap((mismatch) =>
              mismatch.values.map((entry, index) => (
                <tr key={`${mismatch.setting}\n${entry.node_id}`}>
                  {index === 0 ? (
                    <th scope="rowgroup" rowSpan={mismatch.values.length} className="settings-name">
                      {mismatch.setting}
                    </th>
                  ) : null}
                  <td>{entry.node_id}</td>
                  <td>{entry.value === '' ? <span className="settings-muted">not set</span> : <ValueText value={entry.value} />}</td>
                </tr>
              )),
            )}
          </tbody>
        </table>
      </div>
    </section>
  );
}

/** A value as it came: a long one wraps, and scrolls inside its cell where it is longer still. */
function ValueText({ value }: { value: string }) {
  return <span className="settings-value">{value}</span>;
}

interface NodeCardProps {
  node: NodeSettings;
  /** Whether the card shows what the node said. */
  open: boolean;
  /** Says that the card was opened or closed. */
  onOpen: (nodeID: string, open: boolean) => void;
  /** What the settings are filtered by: trimmed, in lower case, or empty. */
  sought: string;
  /** Whether settings that have their default are left out. */
  overriddenOnly: boolean;
  /** The moment the certificates are weighed against. */
  now: number;
}

/**
 * One node: a line that says which it is and whether it answered and, behind
 * it, what it said. A node that did not answer has the line alone.
 */
function NodeCard({ node, open, onOpen, sought, overriddenOnly, now }: NodeCardProps) {
  const head = <NodeHead node={node} />;
  if (!node.answered) {
    return (
      <section className="card card-body panel node-card" aria-label={`Node ${node.node_id}`}>
        <div className="node-card-head">{head}</div>
      </section>
    );
  }
  return (
    <section className="card card-body panel node-card" aria-label={`Node ${node.node_id}`}>
      <details
        open={open}
        onToggle={(event: ToggleEvent<HTMLDetailsElement>) => {
          onOpen(node.node_id, event.currentTarget.open);
        }}
      >
        <summary className="node-card-head" title={open ? 'Close this card' : 'Show what this node said'}>
          <i className="bi bi-chevron-right node-card-mark" aria-hidden="true" />
          {head}
        </summary>
        {/* What a closed card holds is not rendered: a deployment of many
            nodes is then no heavier to show than what is open of it. */}
        {open ? <NodeBody node={node} sought={sought} overriddenOnly={overriddenOnly} now={now} /> : null}
      </details>
    </section>
  );
}

/** The line of a node: its ID, its roles, its build, and whether it answered. */
function NodeHead({ node }: { node: NodeSettings }) {
  const roles = node.roles ?? [];
  // The type of the contract lists the reasons of today; a newer node may
  // give another, which is read as text.
  const reason: string | undefined = node.reason;
  return (
    <>
      <span className="node-card-id">{node.node_id}</span>
      {roles.length === 0 ? null : (
        <span className="grid-badges">
          {roles.map((role) => (
            <span className="badge text-bg-light" key={role}>
              {role}
            </span>
          ))}
        </span>
      )}
      {node.server === undefined ? null : <span className="node-card-build">{describeBuild(node.server).line}</span>}
      {node.answered ? (
        <span className="state state-online">
          <i className="bi bi-check-circle" aria-hidden="true" /> answered
        </span>
      ) : (
        <span className="state state-offline">
          <i className="bi bi-x-circle" aria-hidden="true" /> did not answer: {reasonInWords(reason)}
        </span>
      )}
    </>
  );
}

interface NodeBodyProps {
  node: NodeSettings;
  sought: string;
  overriddenOnly: boolean;
  now: number;
}

/** What a node said of itself. Every part of it may be missing. */
function NodeBody({ node, sought, overriddenOnly, now }: NodeBodyProps) {
  const id = node.node_id;
  const labels = Object.entries(node.labels ?? {});
  const drives = node.drives ?? [];
  const certificates = node.certificates ?? [];
  return (
    <div className="node-card-body">
      <dl className="facts">
        <dt>Started at</dt>
        <dd>{formatTime(node.started_at)}</dd>
        <dt>KMS key ID</dt>
        <dd>{node.kms_key_id === undefined || node.kms_key_id === '' ? '-' : <code>{node.kms_key_id}</code>}</dd>
        <dt>Labels</dt>
        <dd>
          {labels.length === 0 ? (
            '-'
          ) : (
            <span className="grid-badges">
              {labels.map(([name, value]) => (
                <span className="badge text-bg-light" key={name}>
                  {name}={value}
                </span>
              ))}
            </span>
          )}
        </dd>
      </dl>

      <h3>Drives</h3>
      {drives.length === 0 ? <p>This node has no drives.</p> : <DrivesGrid nodeID={id} drives={drives} />}

      {node.metadata === undefined ? null : (
        <>
          <h3>Metadata</h3>
          <MetadataFacts volume={node.metadata} />
        </>
      )}

      <h3>Certificates</h3>
      {certificates.length === 0 ? (
        <p>This node serves no certificate.</p>
      ) : (
        <CertificatesGrid nodeID={id} certificates={certificates} now={now} />
      )}

      <h3>Settings</h3>
      <SettingsGrid nodeID={id} settings={node.settings ?? []} sought={sought} overriddenOnly={overriddenOnly} />
    </div>
  );
}

/** The drives of a node: where each is, how the deployment calls it, what is behind it. */
function DrivesGrid({ nodeID, drives }: { nodeID: string; drives: DriveSettings[] }) {
  return (
    <div className="table-responsive grid-scroll">
      <table className="table table-hover align-middle grid grid-keep grid-wide" aria-label={`Drives of ${nodeID}`}>
        <thead>
          <tr>
            <th scope="col">ID</th>
            <th scope="col">Directory</th>
            <th scope="col">Label</th>
            <th scope="col">File system</th>
            <th scope="col" className="grid-number">
              Size
            </th>
            <th scope="col" className="grid-number">
              Free
            </th>
            <th scope="col">State</th>
            <th scope="col" className="grid-number">
              Erasure set
            </th>
          </tr>
        </thead>
        <tbody>
          {drives.map((drive) => (
            <tr key={drive.id}>
              <td>
                <code title={drive.id}>{shortID(drive.id)}</code>
              </td>
              <td>
                <code>{drive.directory}</code>
              </td>
              <td>{drive.label === undefined || drive.label === '' ? '-' : drive.label}</td>
              <td>{filesystemText(drive.filesystem)}</td>
              <td className="grid-number">{formatBytes(drive.total_bytes)}</td>
              <td className="grid-number">{formatBytes(drive.free_bytes)}</td>
              <td>
                <OnlineState online={drive.online} />
              </td>
              <td className="grid-number">{drive.erasure_set ?? '-'}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** Whether a drive can be used: a sign for the eye and the word beside it. */
function OnlineState({ online }: { online: boolean }) {
  return online ? (
    <span className="state state-online">
      <i className="bi bi-check-circle" aria-hidden="true" /> online
    </span>
  ) : (
    <span className="state state-offline">
      <i className="bi bi-x-circle" aria-hidden="true" /> offline
    </span>
  );
}

/** The directory a node keeps its metadata in. */
function MetadataFacts({ volume }: { volume: SettingsVolume }) {
  return (
    <dl className="stats node-card-stats">
      <div className="stat">
        <dt>Directory</dt>
        <dd>
          <code>{volume.directory}</code>
        </dd>
      </div>
      <div className="stat">
        <dt>File system</dt>
        <dd>{filesystemText(volume.filesystem)}</dd>
      </div>
      <div className="stat">
        <dt>Size</dt>
        <dd>{formatBytes(volume.total_bytes)}</dd>
      </div>
      <div className="stat">
        <dt>Free</dt>
        <dd>{formatBytes(volume.free_bytes)}</dd>
      </div>
    </dl>
  );
}

interface CertificatesGridProps {
  nodeID: string;
  certificates: SettingsCertificate[];
  now: number;
}

/** The certificates a node serves: who each names and until when. */
function CertificatesGrid({ nodeID, certificates, now }: CertificatesGridProps) {
  return (
    <div className="table-responsive grid-scroll">
      <table className="table table-hover align-middle grid grid-keep grid-wide" aria-label={`Certificates of ${nodeID}`}>
        <thead>
          <tr>
            <th scope="col">Use</th>
            <th scope="col">Subject</th>
            <th scope="col">Names</th>
            <th scope="col">Issuer</th>
            <th scope="col">Valid from</th>
            <th scope="col">Valid to</th>
          </tr>
        </thead>
        <tbody>
          {certificates.map((certificate, index) => {
            const names = [...certificate.dns_names, ...certificate.ip_addresses];
            return (
              <tr key={`${String(index)} ${certificate.use} ${certificate.subject}`}>
                <td>{certificate.use}</td>
                <td>{certificate.subject}</td>
                <td>{names.length === 0 ? '-' : names.join(', ')}</td>
                <td>{certificate.issuer}</td>
                <td>{formatTime(certificate.not_before)}</td>
                <td>
                  {formatTime(certificate.not_after)} <Expiry notAfter={certificate.not_after} now={now} />
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

/**
 * Says of a certificate that it has expired, or will within fourteen days,
 * by its last day and the clock of this browser. It is a hint for the
 * reader: which certificate a node accepts is for the nodes to decide.
 */
function Expiry({ notAfter, now }: { notAfter: string; now: number }) {
  const end = new Date(notAfter).getTime();
  if (Number.isNaN(end)) {
    return null;
  }
  if (end <= now) {
    return (
      <span className="state state-offline">
        <i className="bi bi-x-octagon" aria-hidden="true" /> expired
      </span>
    );
  }
  if (end - now <= EXPIRES_SOON_MS) {
    return (
      <span className="state state-warning">
        <i className="bi bi-exclamation-triangle" aria-hidden="true" /> expires soon
      </span>
    );
  }
  return null;
}

interface SettingsGridProps {
  nodeID: string;
  settings: Setting[];
  sought: string;
  overriddenOnly: boolean;
}

/** The settings of a node under their headings, as far as the filter lists them. */
function SettingsGrid({ nodeID, settings, sought, overriddenOnly }: SettingsGridProps) {
  if (settings.length === 0) {
    return <p>This node listed no settings.</p>;
  }
  const listed = settings.filter((setting) => isListed(setting, sought, overriddenOnly));
  if (listed.length === 0) {
    return <p>No setting of this node matches the filter.</p>;
  }
  return (
    <div className="table-responsive grid-scroll">
      <table className="table table-hover align-middle grid grid-keep grid-wide" aria-label={`Settings of ${nodeID}`}>
        <thead>
          <tr>
            <th scope="col" className="grid-name">
              Name
            </th>
            <th scope="col">Value</th>
            <th scope="col">Source</th>
            <th scope="col">Variable</th>
            <th scope="col">Flag</th>
          </tr>
        </thead>
        {groupSettings(listed).map((group) => (
          <tbody key={group.name}>
            <tr className="grid-group">
              <th scope="rowgroup" colSpan={5}>
                {group.name}
              </th>
            </tr>
            {group.settings.map((setting) => (
              <tr key={setting.name}>
                <th scope="row" className="settings-name" title={`Scope: ${setting.scope}`}>
                  {setting.name}
                </th>
                <td>
                  <SettingValue setting={setting} />
                </td>
                <td>
                  <span className={setting.source === 'default' ? 'badge text-bg-light settings-default' : 'badge text-bg-light'}>
                    {setting.source}
                  </span>
                </td>
                <td>{setting.env === undefined || setting.env === '' ? '-' : <code>{setting.env}</code>}</td>
                <td>{setting.flag === undefined || setting.flag === '' ? '-' : <code>--{setting.flag}</code>}</td>
              </tr>
            ))}
          </tbody>
        ))}
      </table>
    </div>
  );
}

/**
 * The value of a setting, as far as it is shown.
 *
 * Defence in depth: the API never sends the value of a `secret` or a
 * `secret-file`, and each node and the answering node redact before this
 * console is told anything. Should a value arrive all the same - or with a
 * kind this console does not know, which the contract says to read as a
 * secret - it is not rendered: such a setting is "set" or "not set" here
 * whatever `value` holds.
 */
function SettingValue({ setting }: { setting: Setting }): ReactNode {
  const notSet = <span className="settings-muted">not set</span>;
  if (!SHOWN_KINDS.has(setting.kind)) {
    const known = setting.kind === 'secret' || setting.kind === 'secret-file';
    const hint = known
      ? 'A secret: only whether it is set is told'
      : `Of a kind this console does not know (${setting.kind}): only whether it is set is told`;
    return <span title={hint}>{setting.set ? 'set' : notSet}</span>;
  }
  if (!setting.set) {
    return notSet;
  }
  if (setting.value === undefined || setting.value === '') {
    return 'set';
  }
  return <ValueText value={setting.value} />;
}
