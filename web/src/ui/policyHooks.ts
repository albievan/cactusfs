import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react';

import {
  api,
  ApiError,
  unwrap,
  type PolicyCatalogue,
  type PolicyFault,
  type PolicyValidation,
  type StatementAnalysis,
} from '../api/client';
import type { PolicyCatalogueView } from '../policy/model';
import { typedPolicy, type TypedPolicy } from './format';
import { useLoad } from './hooks';

// What the policy builder asks of the answering node (ADR-0032, PLAN 9.8):
// the catalogue of the policy language, the buckets to offer as resources,
// and the validation of a document as it is typed. The node says what a
// document may hold, what is wrong with it and what its statements cover;
// nothing here reckons any of it.

/**
 * Whether a view reads no field its generated type does not have, at any
 * depth: through objects, lists and optional fields.
 */
type NoneExtra<View, Generated> = View extends readonly (infer Item)[]
  ? Generated extends readonly (infer Given)[]
    ? NoneExtra<Item, Given>
    : false
  : View extends object
    ? [Exclude<keyof View, keyof Generated>] extends [never]
      ? [
          {
            [K in keyof View & keyof Generated]-?: NoneExtra<NonNullable<View[K]>, NonNullable<Generated[K]>>;
          }[keyof View & keyof Generated],
        ] extends [true]
        ? true
        : false
      : false
    : true;

/**
 * The reader's view of the catalogue (`PolicyCatalogueView`, policy/model.ts)
 * held to the type generated from the contract (`PolicyCatalogue`): the
 * generated catalogue must be a view, and the view may read no field the
 * contract does not have, at any depth. When the two drift this constant
 * does not type, and the build fails.
 */
export type Fits<View, Generated> = [Generated] extends [View] ? NoneExtra<View, Generated> : false;

/** True only while `PolicyCatalogueView` fits the generated `PolicyCatalogue` (see Fits). */
export const CATALOGUE_VIEW_FITS: Fits<PolicyCatalogueView, PolicyCatalogue> = true;

/** What the console says when the node is of a release without the builder (404 `NotFound`). */
export const OLDER_NODE_NOTICE =
  'This node has no policy builder: it is of an older release. Edit the policy as JSON, as before.';

/** What the console says when the caller may not read the catalogue (403 `AccessDenied`). */
export const CATALOGUE_REFUSED_NOTICE =
  'You may not read the catalogue of actions, so the policy builder is not shown. Edit the policy as JSON, as before.';

/** What the console says when the catalogue could not be read for another reason. */
export const CATALOGUE_FAILED_NOTICE =
  'The catalogue of actions could not be read, so the policy builder is not shown. Edit the policy as JSON, as before.';

/** What the console says when the node has a catalogue but cannot check a document (404 `NotFound`). */
export const NO_VALIDATION_NOTICE =
  'This node cannot check a policy before it is saved: the policy is checked when it is saved.';

/** Why a document was not sent: the node's validation refused it. */
export const NOT_SAVED_NOTICE = 'The policy has faults, so it was not saved. Put right what is marked and try again.';

/** The catalogue as far as it was read. */
export type CatalogueState =
  /** Not asked for yet: no policy has been touched (`PolicyTools.want`). The JSON editor works alone. */
  | { status: 'idle' }
  | { status: 'loading' }
  | { status: 'ready'; catalogue: PolicyCatalogue }
  /** Not there for this caller: the JSON editor works alone, as before the builder, with `notice` saying why. */
  | { status: 'unavailable'; why: 'older-node' | 'refused' | 'failed'; notice: string };

/** What the builder of a screen needs of the node, read once for the screen. */
export interface PolicyTools {
  catalogue: CatalogueState;
  /** The names of the buckets the caller may list, to offer as resources; undefined until read, or when they cannot be. */
  buckets: string[] | undefined;
  /**
   * Asks for the catalogue, the first time only: an editor calls it when its
   * policy is first focused or changed. Until then nothing is asked - a
   * caller without `admin:GetPolicyCatalogue` who only looks at a policy
   * causes no refusal in the audit trail.
   */
  want: () => void;
  /** The catalogue's answer, asked for now if it was not yet: what a save decides how to check by. */
  settled: () => Promise<CatalogueState>;
}

/** Whether an error is a node's answer for a path its release does not have: 404 with the code `NotFound`. */
export function isOlderNode(error: unknown): boolean {
  return error instanceof ApiError && error.problem.status === 404 && error.problem.code === 'NotFound';
}

/** Whether an error is the refusal of the catalogue to this caller: 403 with the code `AccessDenied`. */
export function isRefused(error: unknown): boolean {
  return error instanceof ApiError && error.problem.status === 403 && error.problem.code === 'AccessDenied';
}

function loadCatalogue(signal: AbortSignal): Promise<PolicyCatalogue> {
  return unwrap(api.GET('/policy-catalogue', { signal }));
}

/**
 * How long the catalogue is waited for. A node that does not answer by then
 * is taken to have failed to: the JSON editor works alone, and a save is
 * still checked as it is made - it never waits for the catalogue for ever.
 */
export const CATALOGUE_WAIT_MS = 10_000;

/** Why the catalogue is not there, from the error it was refused with. */
function unavailable(error: unknown): CatalogueState {
  if (isOlderNode(error)) {
    return { status: 'unavailable', why: 'older-node', notice: OLDER_NODE_NOTICE };
  }
  if (isRefused(error)) {
    return { status: 'unavailable', why: 'refused', notice: CATALOGUE_REFUSED_NOTICE };
  }
  return { status: 'unavailable', why: 'failed', notice: CATALOGUE_FAILED_NOTICE };
}

/**
 * The catalogue of the answering node, read once it is wanted (`want`) and
 * never before, and, once it is there, the buckets the caller may list. A
 * screen reads them once for all its editors. An editor of one bucket's
 * policy, or a sub-bucket's, names no other bucket: it asks with
 * `listBuckets` false, and no bucket is listed.
 */
export function usePolicyTools(listBuckets = true): PolicyTools {
  const [catalogue, setCatalogue] = useState<CatalogueState>({ status: 'idle' });
  // The one reading of the catalogue, once it was asked for.
  const pending = useRef<Promise<CatalogueState> | null>(null);
  const settled = useCallback((): Promise<CatalogueState> => {
    if (pending.current === null) {
      setCatalogue({ status: 'loading' });
      const stop = new AbortController();
      let timer: ReturnType<typeof setTimeout> | undefined;
      // Unanswered within the wait, the request is given up and the catalogue taken as failed.
      const bounded = new Promise<CatalogueState>((resolve) => {
        timer = setTimeout(() => {
          stop.abort();
          resolve({ status: 'unavailable', why: 'failed', notice: CATALOGUE_FAILED_NOTICE });
        }, CATALOGUE_WAIT_MS);
      });
      const read = loadCatalogue(stop.signal).then(
        (catalogue): CatalogueState => ({ status: 'ready', catalogue }),
        (error: unknown) => unavailable(error),
      );
      void read.finally(() => {
        clearTimeout(timer);
      });
      pending.current = Promise.race([read, bounded]);
      void pending.current.then(setCatalogue);
    }
    return pending.current;
  }, []);
  const want = useCallback(() => {
    void settled();
  }, [settled]);
  const ready = catalogue.status === 'ready' && listBuckets;
  const loadBuckets = useCallback(
    async (signal: AbortSignal): Promise<string[] | undefined> => {
      if (!ready) {
        return undefined;
      }
      const buckets = await unwrap(api.GET('/buckets', { signal }));
      return buckets.map((bucket) => bucket.name);
    },
    [ready],
  );
  const buckets = useLoad(loadBuckets).data;
  return { catalogue, buckets, want, settled };
}

/** The tools a page shares among its policy editors, when it shares them (`PolicyToolsContext.Provider`). */
export const PolicyToolsContext = createContext<PolicyTools | null>(null);

/**
 * The tools of the page, when it gives them, so that the catalogue is read
 * once for all its editors; else the editor's own, read once it is wanted.
 * For the editors of one bucket's policies: no bucket is listed.
 */
export function useSharedPolicyTools(): PolicyTools {
  const shared = useContext(PolicyToolsContext);
  const own = usePolicyTools(false);
  return shared ?? own;
}

/** How long typing must pause before the document is sent to be checked. */
export const VALIDATION_DELAY_MS = 300;

/**
 * Where a document would be stored, which decides where it is checked and
 * how that is authorized: as a named policy or a new key's inline policy
 * (`POST /policy-validation`, the default), as a bucket's policy
 * (`POST /buckets/{bucket}/policy/validation`), or as a sub-bucket's
 * (`POST /buckets/{bucket}/subbuckets/{name}/policy/validation`).
 */
export type ValidationTarget =
  | {
      kind?: 'identity';
      /** The named policy it would be stored as. */
      name?: string;
      /** The user a new API key with it as its inline policy would belong to. */
      keyOwner?: string;
    }
  | { kind: 'bucket'; bucket: string }
  | { kind: 'sub-bucket'; bucket: string; subBucket: string };

/** Where the validation of the document as typed stands. */
export type ValidationState =
  /** Nothing to check: no document, or one that is not a JSON object (the JSON says why), or the builder is off. */
  | { status: 'off' }
  | { status: 'checking' }
  | { status: 'checked'; result: PolicyValidation }
  | { status: 'failed'; error: unknown }
  /** The node has no validation (an older release): the document is checked when it is saved. */
  | { status: 'absent' };

/**
 * What asking the node about a document came to: its answer; that the node
 * has no validation (404 `NotFound`, an older release); or that there was
 * nothing to ask - no document, or validation is not wanted.
 */
export type CheckAnswer =
  | { status: 'checked'; result: PolicyValidation }
  | { status: 'absent' }
  | { status: 'none' };

/** The validation of a document as it is typed. */
export interface Validation {
  state: ValidationState;
  /**
   * The node's answer for the document as it is now, asked at once unless it
   * is there already. Undefined when there is nothing to check, or the node
   * cannot check; rejects when the request fails.
   */
  check: () => Promise<PolicyValidation | undefined>;
  /** As `check`, saying which of the two it was when there is no answer. */
  ask: () => Promise<CheckAnswer>;
}

/** One request that checks a document: what it is known by, and how it is sent. */
interface Check {
  /** Where it goes and its body: two checks with one key are one check. */
  key: string;
  send: (signal?: AbortSignal) => Promise<PolicyValidation>;
}

/** The fields of a target, each a plain value, so that a check is made anew only when one of them changes. */
interface Route {
  kind: 'identity' | 'bucket' | 'sub-bucket';
  name?: string;
  keyOwner?: string;
  bucket: string;
  subBucket: string;
}

function routeOf(target: ValidationTarget): Route {
  switch (target.kind) {
    case 'bucket':
      return { kind: 'bucket', bucket: target.bucket, subBucket: '' };
    case 'sub-bucket':
      return { kind: 'sub-bucket', bucket: target.bucket, subBucket: target.subBucket };
    default:
      return { kind: 'identity', name: target.name, keyOwner: target.keyOwner, bucket: '', subBucket: '' };
  }
}

/**
 * The request that checks a document, as typed - in the body its put sends
 * it in, so that what is checked is byte for byte what a save sends: undefined
 * when it is not a JSON object.
 */
function checkOf(text: string, route: Route): Check | undefined {
  if (text.trim() === '') {
    return undefined;
  }
  // Sent as typed (typedPolicy): a key typed twice reaches the node, which says so.
  try {
    switch (route.kind) {
      case 'bucket': {
        const request = typedPolicy('document', text);
        const bucket = route.bucket;
        return {
          key: `bucket ${bucket}\n${request.body}`,
          send: (signal) =>
            unwrap(
              api.POST('/buckets/{bucket}/policy/validation', {
                params: { path: { bucket } },
                body: { document: request.document },
                bodySerializer: () => request.body,
                signal,
              }),
            ),
        };
      }
      case 'sub-bucket': {
        const request = typedPolicy('policy', text);
        const { bucket, subBucket: name } = route;
        return {
          key: `sub-bucket ${bucket} ${name}\n${request.body}`,
          send: (signal) =>
            unwrap(
              api.POST('/buckets/{bucket}/subbuckets/{name}/policy/validation', {
                params: { path: { bucket, name } },
                body: { policy: request.document },
                bodySerializer: () => request.body,
                signal,
              }),
            ),
        };
      }
      default: {
        const request: TypedPolicy = typedPolicy('document', text, { name: route.name, key_owner: route.keyOwner });
        return {
          key: `identity\n${request.body}`,
          send: (signal) =>
            unwrap(
              api.POST('/policy-validation', {
                body: { document: request.document },
                bodySerializer: () => request.body,
                signal,
              }),
            ),
        };
      }
    }
  } catch {
    return undefined;
  }
}

interface Answer {
  /** The key of the check it answers. */
  key: string;
  result?: PolicyValidation;
  error?: unknown;
}

/**
 * Checks a document as it is typed, where it would be stored (`target`):
 * once typing has paused for VALIDATION_DELAY_MS, and again whenever it
 * changes; a check that is no longer wanted is given up. Nothing is checked
 * while `enabled` is false.
 */
export function usePolicyValidation(text: string, target: ValidationTarget, enabled: boolean): Validation {
  const { kind, name, keyOwner, bucket, subBucket } = routeOf(target);
  const request = useMemo(
    () => (enabled ? checkOf(text, { kind, name, keyOwner, bucket, subBucket }) : undefined),
    [enabled, text, kind, name, keyOwner, bucket, subBucket],
  );
  const [answer, setAnswer] = useState<Answer | undefined>(undefined);
  // The check the node answered 404 to, by its key: that check is not asked
  // again, but the next one is - one node of an older release met during a
  // rolling upgrade does not turn validation off for the editor.
  const [absentKey, setAbsentKey] = useState<string | undefined>(undefined);
  const absent = request !== undefined && absentKey === request.key;
  // The key of the check of the text as it is now: an answer that comes for
  // another is not kept.
  const current = useRef<string | undefined>(undefined);
  useEffect(() => {
    current.current = request?.key;
  }, [request]);

  useEffect(() => {
    if (request === undefined || absent) {
      return undefined;
    }
    const stop = new AbortController();
    const timer = setTimeout(() => {
      request.send(stop.signal).then(
        (result) => {
          if (!stop.signal.aborted) {
            setAnswer({ key: request.key, result });
          }
        },
        (error: unknown) => {
          if (stop.signal.aborted) {
            return;
          }
          if (isOlderNode(error)) {
            setAbsentKey(request.key);
          } else {
            setAnswer({ key: request.key, error });
          }
        },
      );
    }, VALIDATION_DELAY_MS);
    return () => {
      clearTimeout(timer);
      stop.abort();
    };
  }, [request, absent]);

  const ask = useCallback(async (): Promise<CheckAnswer> => {
    if (request === undefined) {
      return { status: 'none' };
    }
    if (absent) {
      return { status: 'absent' };
    }
    if (answer?.key === request.key && answer.result !== undefined) {
      return { status: 'checked', result: answer.result };
    }
    const keep = (next: Answer) => {
      if (current.current === request.key) {
        setAnswer(next);
      }
    };
    try {
      const result = await request.send();
      keep({ key: request.key, result });
      return { status: 'checked', result };
    } catch (error) {
      if (isOlderNode(error)) {
        if (current.current === request.key) {
          setAbsentKey(request.key);
        }
        return { status: 'absent' };
      }
      keep({ key: request.key, error });
      throw error;
    }
  }, [request, absent, answer]);

  const check = useCallback(async (): Promise<PolicyValidation | undefined> => {
    const asked = await ask();
    return asked.status === 'checked' ? asked.result : undefined;
  }, [ask]);

  let state: ValidationState;
  if (absent) {
    state = { status: 'absent' };
  } else if (request === undefined) {
    state = { status: 'off' };
  } else if (answer?.key !== request.key) {
    state = { status: 'checking' };
  } else if (answer.result !== undefined) {
    state = { status: 'checked', result: answer.result };
  } else {
    state = { status: 'failed', error: answer.error };
  }
  return { state, check, ask };
}

/**
 * Asks the node to check the document as it is now, and refuses to go on -
 * so that nothing is sent - when the node finds a fault in it, or cannot be
 * asked. When there is nothing the node can check, it goes on: the save is
 * checked when it is made, as before the builder. It gives the node's
 * answer, when there is one, for what it says of the statements.
 */
export async function requireValid(validation: Validation): Promise<PolicyValidation | undefined> {
  const result = await validation.check();
  if (result !== undefined && !result.valid) {
    throw new Error(NOT_SAVED_NOTICE);
  }
  return result;
}

/**
 * Asks the node once to check a document where it would be stored, as typed
 * and in the body its put sends - for an editor that does not check as it
 * is typed, at the moment of saving. Rejects when the request fails other
 * than with 404 `NotFound`, which is `absent`.
 */
export async function checkOnce(text: string, target: ValidationTarget): Promise<CheckAnswer> {
  const check = checkOf(text, routeOf(target));
  if (check === undefined) {
    return { status: 'none' };
  }
  try {
    return { status: 'checked', result: await check.send() };
  } catch (error) {
    if (isOlderNode(error)) {
      return { status: 'absent' };
    }
    throw error;
  }
}

/** Why a save was not sent: the text was changed while it was being checked. */
export const CHANGED_WHILE_CHECKED =
  'The policy was changed while it was being checked, so it was not saved. Save it again.';

/**
 * How a save is checked: as typed, with the builder; once, as it is saved;
 * or not at all.
 */
export type Checking = 'as-typed' | 'at-save' | 'none';

/**
 * How a save of a bucket's or a sub-bucket's policy is checked, by the
 * catalogue's answer: as typed with the builder; once, as it is saved,
 * without it (the catalogue refused, failed or not answered in time). On a
 * node of an older release (catalogue 404) a bucket's policy is checked at
 * save as well (`olderNodeChecked`) - a 404 then is asked about, since such
 * a node cannot say whether the policy opens the bucket to anyone - while
 * a sub-bucket's, which can open nothing to anyone, is not checked at all.
 */
export function checkingOf(catalogue: CatalogueState, olderNodeChecked = false): Checking {
  if (catalogue.status === 'ready') {
    return 'as-typed';
  }
  if (catalogue.status === 'unavailable' && catalogue.why === 'older-node') {
    return olderNodeChecked ? 'at-save' : 'none';
  }
  return 'at-save';
}

/**
 * How a save of a named policy or a key's inline policy is checked: as
 * typed with the builder, and not at all without it - the fallback of 9.8,
 * in which nothing is asked of a node that gives this caller no builder.
 */
export function identityCheckingOf(catalogue: CatalogueState): Checking {
  return catalogue.status === 'ready' ? 'as-typed' : 'none';
}

/**
 * Checks a named policy or a key's inline policy as it is saved, once the
 * catalogue has answered (waited for if it is loading), and refuses to go
 * on when the node finds a fault in it, or cannot be asked: what is checked
 * is the exact text that is sent, even when the catalogue came in while the
 * save was being made.
 */
export async function requireValidAtSave(
  tools: PolicyTools,
  validation: Validation,
  text: string,
  target: ValidationTarget,
): Promise<void> {
  const asked = await askAtSave(identityCheckingOf(await tools.settled()), validation, text, target);
  if (asked.status === 'checked' && !asked.result.valid) {
    throw new Error(NOT_SAVED_NOTICE);
  }
}

/**
 * Asks the node about a document as a save is made: the answer as typed
 * when it checks as typed (asked once more if that had nothing to ask - the
 * catalogue came in while the save was being made), once otherwise, and
 * nothing on a node of an older release.
 */
export async function askAtSave(
  checking: Checking,
  validation: Validation,
  text: string,
  target: ValidationTarget,
): Promise<CheckAnswer> {
  if (checking === 'none') {
    return { status: 'none' };
  }
  if (checking === 'as-typed') {
    const asked = await validation.ask();
    if (asked.status !== 'none') {
      return asked;
    }
  }
  return checkOnce(text, target);
}

/**
 * The statements the node says are open to anyone (`open_to_anyone`), in
 * the order of its answer. The console does not reckon it: a statement the
 * node does not mark is not among them.
 */
export function openToAnyone(result: PolicyValidation | undefined): StatementAnalysis[] {
  return (result?.statements ?? []).filter((statement) => statement.open_to_anyone === true);
}

/** The refusal of the last save of one editor, with the text it refused. */
export interface Refusal {
  /** The faults the API refused `text` with, when it is the text that was refused. */
  faultsFor: (text: string) => PolicyFault[] | undefined;
  /** Keeps what the API refused `text` with; an error without faults clears what was kept. */
  keep: (text: string, error: unknown) => void;
}

/**
 * The faults of the last refused save of one editor, kept with the text it
 * sent, so that they are shown at that editor while that text is the one
 * typed - and never at another editor of the screen.
 */
export function useRefusal(): Refusal {
  const [refused, setRefused] = useState<{ text: string; faults: PolicyFault[] } | null>(null);
  const keep = useCallback((text: string, error: unknown) => {
    const faults = faultsOf(error);
    setRefused(faults === undefined ? null : { text, faults });
  }, []);
  return { faultsFor: (text) => (refused?.text === text ? refused.faults : undefined), keep };
}

/** The faults the API gave with its refusal of a document (`Problem.faults`), if it gave any. */
export function faultsOf(error: unknown): PolicyFault[] | undefined {
  if (!(error instanceof ApiError)) {
    return undefined;
  }
  const faults = error.problem.faults;
  return faults === undefined || faults.length === 0 ? undefined : faults;
}
