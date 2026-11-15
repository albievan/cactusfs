import { useId, useLayoutEffect, useState, type ReactNode, type RefObject } from 'react';

import { ApiError, type Problem } from '../api/client';
import { ProblemNotice } from './ProblemNotice';

/**
 * The code of the problem the API answers a path it does not know with
 * (`internal/adapters/adminapi/handler.go`, "No such endpoint"). A bucket,
 * a sub-bucket, a policy or a tag that is not there has codes of its own
 * (`NoSuchBucket`, `NoSuchSubBucket`, `NoSuchEntity`): only a node of a
 * release before these operations - met during a rolling upgrade - answers
 * them `NotFound`.
 */
export const NO_SUCH_ENDPOINT = 'NotFound';

/** A problem the console says something more of, or undefined. */
export type Explain = (problem: Problem) => string | undefined;

/** Whether the API refused to tell the caller this: 403. */
export function refused(error: unknown): boolean {
  return error instanceof ApiError && error.problem.status === 403;
}

/** What is said of a 404 `NotFound`: the node that answered is of an older release. */
export function olderNode(what: string): Explain {
  return (problem) =>
    problem.status === 404 && problem.code === NO_SUCH_ENDPOINT
      ? `${what} not available on the node that answered (an older release); try again.`
      : undefined;
}

/** The first of the explanations that has something to say. */
export function firstOf(...explains: Explain[]): Explain {
  return (problem) => {
    for (const explain of explains) {
      const words = explain(problem);
      if (words !== undefined) {
        return words;
      }
    }
    return undefined;
  };
}

/** What is said when the API answers a code, and the words. */
export function onCode(code: string, words: string): Explain {
  return (problem) => (problem.code === code ? words : undefined);
}

interface FoldProps {
  /** The heading, and the name of the region. */
  title: string;
  /** What is shown, for the hint of the button: "the policy of this bucket". */
  what: string;
  children: ReactNode;
}

/**
 * A part of a bucket's details that is folded until it is asked for. What it
 * holds is made - and so asks the API - only once it is unfolded, and is
 * kept, with what was typed into it, when it is folded again.
 */
export function Fold({ title, what, children }: FoldProps) {
  const headingId = useId();
  const bodyId = useId();
  const [open, setOpen] = useState(false);
  const [made, setMade] = useState(false);
  return (
    <section className="bucket-section" aria-labelledby={headingId}>
      <h3 id={headingId} className="bucket-section-heading">
        <button
          type="button"
          className="bucket-details-toggle"
          aria-expanded={open}
          aria-controls={bodyId}
          title={open ? `Hide ${what}` : `Show ${what}`}
          onClick={() => {
            setOpen(!open);
            setMade(true);
          }}
        >
          <i className={`bi ${open ? 'bi-chevron-down' : 'bi-chevron-right'}`} aria-hidden="true" />
          {title}
        </button>
      </h3>
      <div id={bodyId} className="bucket-section-body" hidden={!open}>
        {made ? children : null}
      </div>
    </section>
  );
}

interface LoadFailureProps {
  error: unknown;
  explain: Explain;
  onRetry: () => void;
  /** What the button's hint says is asked for again. */
  what: string;
}

/** Why something could not be read, in the API's words, and a way to ask again. */
export function LoadFailure({ error, explain, onRetry, what }: LoadFailureProps) {
  return (
    <div className="bucket-section-failure">
      <ProblemNotice error={error} explain={explain} />
      <button type="button" className="btn btn-outline-secondary btn-sm" onClick={onRetry} title={`Ask for ${what} again`}>
        <i className="bi bi-arrow-clockwise" aria-hidden="true" /> <span className="btn-text">Try again</span>
      </button>
    </div>
  );
}

/**
 * Where the keyboard goes after a change: a save or a removal makes a fresh
 * form, and the button it was on goes with the old one. Once `askedRef` is
 * set, the Save button of the form inside `areaRef` takes it. The change
 * lands after an answer of the API, outside any event, where React would run
 * a plain effect after the fresh form is shown: a layout effect moves the
 * keyboard in the commit that shows it.
 */
export function useFocusAfterChange(areaRef: RefObject<HTMLDivElement | null>, askedRef: RefObject<boolean>) {
  useLayoutEffect(() => {
    if (!askedRef.current) {
      return;
    }
    const save = areaRef.current?.querySelector<HTMLButtonElement>('button[type="submit"]');
    if (save !== null && save !== undefined) {
      askedRef.current = false;
      save.focus();
    }
  });
}

/** A region that says how the last change ended, in the page before there is anything to say, so that it is announced. */
export function Said({ text }: { text: string | null }) {
  return (
    <div className="notice-region" role="status">
      {text === null ? null : <p className="tag-said">{text}</p>}
    </div>
  );
}
