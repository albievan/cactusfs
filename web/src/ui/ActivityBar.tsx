import { useEffect, useRef, useState, type KeyboardEvent } from 'react';

import { useDeletionList, useDeletions } from '../deletions/context';
import { doneOf, progressLineOf, totalOf, type Deletion } from '../deletions/state';
import { useNoticeActions, useNotices } from '../notices/context';
import type { NoticeKind } from '../notices/state';
import { routeHref } from '../router';
import { useUploadList, useUploads } from '../uploads/context';
import { batchesInFlight, batchesOf, NO_RUN, ofBatches, runAfter, totalsOf, type UploadTotals } from '../uploads/state';
import { UploadTile } from '../uploads/UploadTile';
import { formatBytes, formatTime } from './format';

/** What the bar has to say of the uploads: nothing, that they are on their way, or how they ended. */
type Phase = 'idle' | 'active' | 'stored' | 'incomplete';

function phaseOf(totals: UploadTotals): Phase {
  if (totals.files === 0) {
    return 'idle';
  }
  if (totals.inFlight > 0) {
    return 'active';
  }
  return totals.stored === totals.files ? 'stored' : 'incomplete';
}

const PHASE_ICON: Record<Phase, string> = {
  idle: 'bi-cloud-check',
  active: 'bi-arrow-up-circle',
  stored: 'bi-check-circle',
  incomplete: 'bi-exclamation-triangle',
};

function files(count: number): string {
  return count === 1 ? '1 file' : `${String(count)} files`;
}

/**
 * What is said of the files that failed: how many were not stored and, apart
 * from them, how many the server was still working on when the console
 * stopped asking - of those nobody knows that they are not stored.
 */
function failedOf(totals: UploadTotals): string[] {
  const parts: string[] = [];
  if (totals.failed > totals.undecided) {
    parts.push(`${String(totals.failed - totals.undecided)} not stored`);
  }
  if (totals.undecided > 0) {
    parts.push(`${String(totals.undecided)} not confirmed`);
  }
  return parts;
}

/** How the uploads ended: "39 of 39 stored", or "37 stored, 2 not stored". */
function outcomeOf(totals: UploadTotals): string {
  if (totals.stored === totals.files) {
    return `${String(totals.stored)} of ${String(totals.files)} stored`;
  }
  const parts = [`${String(totals.stored)} stored`, ...failedOf(totals)];
  if (totals.cancelled > 0) {
    parts.push(`${String(totals.cancelled)} cancelled`);
  }
  return parts.join(', ');
}

/**
 * The line of the bar, as it is shown. While files are on their way it
 * speaks of them - `now`, the batches that have a file on its way - and
 * not of what ended earlier in the visit; when none is, of everything that
 * was not dismissed.
 */
function summaryOf(phase: Phase, totals: UploadTotals, now: UploadTotals): string {
  switch (phase) {
    case 'idle':
      return 'No uploads in progress';
    case 'active':
      return [
        `Uploading ${files(now.files)}`,
        `${String(now.stored)} stored`,
        `${formatBytes(now.bytesSent)} of ${formatBytes(now.bytesTotal)}`,
        ...failedOf(now),
      ].join(' · ');
    case 'stored':
    case 'incomplete':
      return outcomeOf(totals);
  }
}

/**
 * What is read out. It says that uploads began, that a file was not stored,
 * and how they ended - and holds none of the figures that change as the
 * files progress, so that it changes, and is read out, only then.
 *
 * While files are on their way it speaks of `now`, as the line does. When
 * none is, of `run`: the batches that were on their way together until just
 * now, as far as they were not dismissed. A file that failed earlier in the
 * visit is then no part of how these ended.
 */
function announcementOf(active: boolean, now: UploadTotals, run: UploadTotals): string {
  if (active) {
    return [`Uploading ${files(now.files)}`, ...failedOf(now)].map((part) => `${part}.`).join(' ');
  }
  return run.files === 0 ? '' : `Uploads finished: ${outcomeOf(run)}.`;
}

/**
 * What the bar says of the deletions on their way, in the words of their
 * progress - "Deleting 1 200 of 5 000 from photos" - or null when none is.
 * `withBucket` false names no bucket, for the bar of a locked session.
 */
function deletingOf(running: Deletion[], withBucket: boolean): string | null {
  return running.length === 0 ? null : running.map((d) => progressLineOf(d, withBucket)).join(' · ');
}

/** How far the deletions on their way are together, from 0 to 1, as far as it is known. */
function deletedShare(running: Deletion[]): number {
  const total = running.reduce((sum, d) => sum + totalOf(d).count, 0);
  return total === 0 ? 0 : Math.min(1, running.reduce((sum, d) => sum + doneOf(d), 0) / total);
}

/**
 * What is read out of the deletions: that they are on their way, and from
 * which buckets - no figure that changes as objects are deleted. How they
 * ended is a notice.
 */
function deletingAnnounced(running: Deletion[], withBucket: boolean): string {
  if (running.length === 0) {
    return '';
  }
  if (!withBucket) {
    return 'Deleting objects.';
  }
  return running.map((d) => `Deleting objects from ${d.bucket}.`).join(' ');
}

/**
 * The question asked before signing out: of the files still on their way,
 * of the objects still being deleted, or of both - signing out stops them.
 */
function leavingQuestionOf(filesInFlight: number, deleting: boolean): string {
  const files = filesInFlight === 1 ? '1 file is' : `${String(filesInFlight)} files are`;
  if (filesInFlight > 0 && deleting) {
    return `${files} still being uploaded and objects are being deleted. Sign out and stop both?`;
  }
  if (filesInFlight > 0) {
    return `${files} still being uploaded. Sign out and stop the uploads?`;
  }
  if (deleting) {
    return 'Objects are still being deleted. Sign out and stop the deletion?';
  }
  return 'The uploads are over. Sign out?';
}

const NOTICE_ICON: Record<NoticeKind, string> = {
  success: 'bi-check-circle',
  error: 'bi-exclamation-triangle',
  info: 'bi-info-circle',
};

// The kind of a notice in a word, before its title: not by colour alone.
const NOTICE_WORD: Record<NoticeKind, string> = { success: 'Done', error: 'Not done', info: 'Note' };

type Panel = 'uploads' | 'notices';

/** A question the bar asks before the session is ended while files are on their way. */
export interface LeavingQuestion {
  /** Ends the session. */
  onLeave: () => void;
  /** Stays. */
  onStay: () => void;
}

interface ActivityBarProps {
  /**
   * Set while the console asks whether to sign out with uploads on their
   * way. Escape answers it with `onStay`; where the keyboard goes then is
   * for whoever asked.
   */
  leaving?: LeavingQuestion | null;
  /**
   * Set where the bar only says how the uploads stand: on the page of a
   * locked session. Nothing of it can then be operated - it has no button,
   * opens nothing and asks nothing - and it names no bucket and no object:
   * the line, the bar for all the files and what is read out are all there
   * is. A question in `leaving` is not asked.
   */
  readOnly?: boolean;
}

/**
 * The bar at the top of every screen, always in view: what the console is
 * doing and what it did. It is always there, in the same place - when
 * nothing is on its way it says so, in a line - so that where to look never
 * changes.
 *
 * While files are sent it says how many, how much, and shows one bar for all
 * of them; when they are over, what was stored and what was not, until that
 * is dismissed. While objects are deleted it says how many of how many, with
 * a button that cancels what has not begun; how a deletion ended is a
 * notice. "Uploads" opens every file with its bucket and its own bar;
 * "Notices" opens what happened in this visit, newest first.
 *
 * Nothing here takes the keyboard away from where it is: a notice that
 * arrives changes a count, and what is read out is a polite live region.
 *
 * Read-only, it is not that bar but what it says: see `readOnly`.
 */
export function ActivityBar({ leaving: asked = null, readOnly = false }: ActivityBarProps) {
  // A bar that cannot be operated asks nothing.
  const leaving = readOnly ? null : asked;
  const uploads = useUploads();
  const list = useUploadList();
  const deletions = useDeletions();
  const deleting = useDeletionList().filter((d) => d.running);
  const notices = useNotices();
  const { dismiss, dismissAll } = useNoticeActions();
  const [open, setOpen] = useState<Panel | null>(null);
  const uploadsButton = useRef<HTMLButtonElement>(null);
  const noticesButton = useRef<HTMLButtonElement>(null);
  const stayButton = useRef<HTMLButtonElement>(null);

  const totals = totalsOf(list);
  const phase = phaseOf(totals);
  const asking = leaving !== null;
  // What is on its way now, and what was on its way together until just
  // now: the line and what is read out speak of these, not of every upload
  // of the visit. The run is remembered from one drawing to the next.
  const flying = batchesInFlight(list);
  const [run, setRun] = useState(NO_RUN);
  const current = runAfter(run, flying);
  if (current !== run) {
    setRun(current);
  }
  const now = totalsOf(ofBatches(list, flying));
  // The deletions on their way are said after the uploads, or in their place
  // when nothing is uploaded.
  const deletingLine = deletingOf(deleting, !readOnly);
  const uploadsLine = summaryOf(phase, totals, now);
  const line =
    deletingLine === null ? uploadsLine : phase === 'idle' ? deletingLine : `${uploadsLine} · ${deletingLine}`;
  const deletingOnly = deletingLine !== null && phase === 'idle';

  // The question was asked for with a button far from here: the keyboard
  // goes to the answer that changes nothing.
  useEffect(() => {
    if (asking) {
      stayButton.current?.focus();
    }
  }, [asking]);

  const toggle = (panel: Panel) => {
    setOpen((shown) => (shown === panel ? null : panel));
  };

  // Escape answers the question, if one is asked, with the answer that
  // changes nothing. Otherwise it closes the panel that is open, and the
  // keyboard is back on the button that opened it.
  const onKeyDown = (event: KeyboardEvent<HTMLElement>) => {
    if (event.key !== 'Escape') {
      return;
    }
    if (leaving !== null) {
      leaving.onStay();
    } else if (open !== null) {
      (open === 'uploads' ? uploadsButton : noticesButton).current?.focus();
      setOpen(null);
    }
  };

  return (
    <section
      className={`${readOnly ? 'activitybar-readonly' : 'activitybar'} activitybar-is-${deletingOnly ? 'active' : phase}`}
      aria-label="Activity"
      onKeyDown={readOnly ? undefined : onKeyDown}
    >
      <div className="activitybar-line">
        <p className="activitybar-summary">
          <i className={`bi ${deletingOnly ? 'bi-trash' : PHASE_ICON[phase]}`} aria-hidden="true" />
          {/* On a wide screen the line is cut short where its room ends: all of it is in the hint. */}
          <span className="activitybar-text" title={line}>
            {line}
          </span>
        </p>
        {phase === 'active' ? (
          <progress
            className="upload-bar activitybar-progress"
            aria-label="All uploads"
            value={now.bytesTotal > 0 ? now.bytesSent / now.bytesTotal : 0}
            max={1}
          />
        ) : null}
        {phase !== 'active' && deleting.length > 0 ? (
          <progress
            className="upload-bar activitybar-progress"
            aria-label="All deletions"
            value={deletedShare(deleting)}
            max={1}
          />
        ) : null}
        <p className="visually-hidden activitybar-live" aria-live="polite">
          {[
            announcementOf(phase === 'active', now, totalsOf(ofBatches(list, current.batches))),
            deletingAnnounced(deleting, !readOnly),
          ]
            .filter((part) => part !== '')
            .join(' ')}
        </p>
        {readOnly ? null : (
          <div className="activitybar-buttons">
            {deleting.length > 0 ? (
              <button
                type="button"
                className="btn btn-outline-danger btn-sm"
                onClick={deletions.cancelAll}
                title="Stop deleting: what has not begun is not sent, what is on its way is answered and counted"
              >
                <i className="bi bi-stop-circle" aria-hidden="true" /> <span className="btn-text">Cancel deletion</span>
              </button>
            ) : null}
            <button
              ref={uploadsButton}
              type="button"
              className="btn btn-outline-secondary btn-sm"
              aria-label={`Uploads: ${String(totals.files)}`}
              aria-expanded={open === 'uploads'}
              aria-controls="activity-uploads"
              onClick={() => {
                toggle('uploads');
              }}
              title="Show every upload of this visit: its bucket, how far it is, and why if it was not stored"
            >
              <i className="bi bi-upload" aria-hidden="true" /> <span className="btn-text">Uploads</span>
              <span className="badge activitybar-count">{totals.files}</span>
            </button>
            <button
              ref={noticesButton}
              type="button"
              className="btn btn-outline-secondary btn-sm"
              aria-label={`Notices: ${String(notices.length)}`}
              aria-expanded={open === 'notices'}
              aria-controls="activity-notices"
              onClick={() => {
                toggle('notices');
              }}
              title="Show what happened in this visit, newest first"
            >
              <i className="bi bi-bell" aria-hidden="true" /> <span className="btn-text">Notices</span>
              <span className="badge activitybar-count">{notices.length}</span>
            </button>
          </div>
        )}
      </div>

      {readOnly || (leaving === null && open === null) ? null : (
        <div className="activitybar-drop">
          {leaving === null ? null : (
            <div
              className="confirm activitybar-leaving"
              role="group"
              aria-label={deleting.length > 0 ? 'Sign out and stop what is on its way?' : 'Sign out and stop the uploads?'}
            >
              <span className="confirm-question">{leavingQuestionOf(totals.inFlight, deleting.length > 0)}</span>
              <button
                type="button"
                className="btn btn-outline-danger btn-sm"
                onClick={leaving.onLeave}
                title="Stop what is on its way and end this session: what was not stored or deleted yet is not"
              >
                <i className="bi bi-box-arrow-right" aria-hidden="true" /> <span className="btn-text">Yes, sign out</span>
              </button>
              <button
                ref={stayButton}
                type="button"
                className="btn btn-outline-secondary btn-sm"
                onClick={leaving.onStay}
                title="Stay signed in: the uploads go on"
              >
                <i className="bi bi-x-lg" aria-hidden="true" /> <span className="btn-text">Stay signed in</span>
              </button>
            </div>
          )}

          {open === 'uploads' ? (
            <div id="activity-uploads" className="activitybar-panel" role="group" aria-label="Uploads">
              {list.length === 0 ? <p className="activitybar-empty">There are no uploads to show.</p> : null}
              {batchesOf(list).map((batch) => {
                const of = totalsOf(batch.uploads);
                return (
                  <section key={batch.id} className="activitybar-batch" aria-label={`Upload to ${batch.bucket}`}>
                    <p className="uploads-summary">
                      <a
                        className="btn btn-outline-secondary btn-sm"
                        href={routeHref(['buckets', batch.bucket])}
                        title="Open the bucket these files go to"
                      >
                        <i className="bi bi-bucket" aria-hidden="true" />
                        {batch.bucket}
                      </a>
                      <strong>
                        {of.stored} of {of.files} stored
                      </strong>
                      {failedOf(of).map((part) => (
                        <span key={part} className="uploads-failed">
                          {' '}
                          · {part}
                        </span>
                      ))}
                      {of.cancelled > 0 ? <span className="uploads-cancelled"> · {of.cancelled} cancelled</span> : null}
                      {of.failed > 0 ? (
                        <button
                          type="button"
                          className="btn btn-outline-danger btn-sm"
                          onClick={() => {
                            uploads.retryFailed({ batch: batch.id });
                          }}
                          title="Send again the files that were not stored"
                        >
                          <i className="bi bi-arrow-repeat" aria-hidden="true" /> <span className="btn-text">Retry failed</span>
                        </button>
                      ) : null}
                      {of.files > of.inFlight ? (
                        <button
                          type="button"
                          className="btn btn-outline-secondary btn-sm"
                          onClick={() => {
                            uploads.dismissFinished({ batch: batch.id });
                          }}
                          title="Take the files that are over off this list: those still on their way stay"
                        >
                          <i className="bi bi-x-lg" aria-hidden="true" /> <span className="btn-text">Dismiss finished</span>
                        </button>
                      ) : null}
                    </p>
                    <ul className="uploads" aria-label={`Files going to ${batch.bucket}`}>
                      {batch.uploads.map((upload) => (
                        <UploadTile
                          key={upload.id}
                          upload={upload}
                          barLabel={`Upload of ${upload.key} to ${upload.bucket}`}
                          onCancel={uploads.cancel}
                        />
                      ))}
                    </ul>
                  </section>
                );
              })}
            </div>
          ) : null}

          {open === 'notices' ? (
            <div id="activity-notices" className="activitybar-panel" role="group" aria-label="Notices">
              {notices.length === 0 ? (
                <p className="activitybar-empty">There are no notices to show.</p>
              ) : (
                <>
                  <p className="uploads-summary">
                    <strong>
                      {notices.length === 1 ? '1 notice' : `${String(notices.length)} notices`}, newest first
                    </strong>
                    <button
                      type="button"
                      className="btn btn-outline-secondary btn-sm"
                      onClick={dismissAll}
                      title="Forget every notice of this visit"
                    >
                      <i className="bi bi-x-lg" aria-hidden="true" /> <span className="btn-text">Dismiss all</span>
                    </button>
                  </p>
                  <ul className="activitybar-notices">
                    {notices.map((notice) => (
                      <li key={notice.id} className={`activitybar-notice activitybar-notice-${notice.kind}`}>
                        <i className={`bi ${NOTICE_ICON[notice.kind]}`} aria-hidden="true" />
                        <div className="activitybar-notice-body">
                          <p className="activitybar-notice-title">
                            <span className="activitybar-notice-kind">{NOTICE_WORD[notice.kind]}:</span> {notice.title}
                          </p>
                          {notice.detail === undefined || notice.detail === '' ? null : (
                            <p className="activitybar-notice-detail">{notice.detail}</p>
                          )}
                          <p className="activitybar-notice-meta">{formatTime(notice.at)}</p>
                        </div>
                        {notice.link === undefined ? null : (
                          <a className="btn btn-outline-secondary btn-sm" href={notice.link.href} title="Go to what this notice is about">
                            <i className="bi bi-box-arrow-in-right" aria-hidden="true" />{' '}
                            <span className="btn-text">{notice.link.label}</span>
                          </a>
                        )}
                        <button
                          type="button"
                          className="btn btn-outline-secondary btn-sm"
                          aria-label={`Dismiss: ${notice.title}`}
                          onClick={() => {
                            dismiss(notice.id);
                          }}
                          title="Forget this notice"
                        >
                          <i className="bi bi-x-lg" aria-hidden="true" />
                        </button>
                      </li>
                    ))}
                  </ul>
                </>
              )}
            </div>
          ) : null}
        </div>
      )}
    </section>
  );
}
