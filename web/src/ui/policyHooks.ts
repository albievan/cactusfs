import { useCallback, useEffect, useMemo, useState } from 'react';

import {
  api,
  ApiError,
  unwrap,
  type PolicyCatalogue,
  type PolicyFault,
  type PolicyValidation,
} from '../api/client';
import type { PolicyCatalogueView } from '../policy/model';
import { typedPolicy, type TypedPolicy } from './format';
import { useLoad, type Loaded } from './hooks';

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
  | { status: 'loading' }
  | { status: 'ready'; catalogue: PolicyCatalogue }
  /** Not there for this caller: the JSON editor works alone, as before the builder, with `notice` saying why. */
  | { status: 'unavailable'; why: 'older-node' | 'refused' | 'failed'; notice: string };

/** What the builder of a screen needs of the node, read once for the screen. */
export interface PolicyTools {
  catalogue: CatalogueState;
  /** The names of the buckets the caller may list, to offer as resources; undefined until read, or when they cannot be. */
  buckets: string[] | undefined;
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

function catalogueState(loaded: Loaded<PolicyCatalogue>): CatalogueState {
  if (loaded.data !== undefined) {
    return { status: 'ready', catalogue: loaded.data };
  }
  if (loaded.loading) {
    return { status: 'loading' };
  }
  if (isOlderNode(loaded.error)) {
    return { status: 'unavailable', why: 'older-node', notice: OLDER_NODE_NOTICE };
  }
  if (isRefused(loaded.error)) {
    return { status: 'unavailable', why: 'refused', notice: CATALOGUE_REFUSED_NOTICE };
  }
  return { status: 'unavailable', why: 'failed', notice: CATALOGUE_FAILED_NOTICE };
}

/**
 * The catalogue of the answering node and, once it is there, the buckets the
 * caller may list. A screen reads them once for all its editors.
 */
export function usePolicyTools(): PolicyTools {
  const catalogue = catalogueState(useLoad(loadCatalogue));
  const ready = catalogue.status === 'ready';
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
  return { catalogue, buckets };
}

/** How long typing must pause before the document is sent to be checked. */
export const VALIDATION_DELAY_MS = 300;

/** Where a document would be stored, which decides how its validation is authorized. */
export interface ValidationTarget {
  /** The named policy it would be stored as. */
  name?: string;
  /** The user a new API key with it as its inline policy would belong to. */
  keyOwner?: string;
}

/** Where the validation of the document as typed stands. */
export type ValidationState =
  /** Nothing to check: no document, or one that is not a JSON object (the JSON says why), or the builder is off. */
  | { status: 'off' }
  | { status: 'checking' }
  | { status: 'checked'; result: PolicyValidation }
  | { status: 'failed'; error: unknown }
  /** The node has no validation (an older release): the document is checked when it is saved. */
  | { status: 'absent' };

/** The validation of a document as it is typed. */
export interface Validation {
  state: ValidationState;
  /**
   * The node's answer for the document as it is now, asked at once unless it
   * is there already. Undefined when there is nothing to check, or the node
   * cannot check; rejects when the request fails.
   */
  check: () => Promise<PolicyValidation | undefined>;
}

/** The request that checks a document, as typed: undefined when it is not a JSON object. */
function requestOf(text: string, name: string | undefined, keyOwner: string | undefined): TypedPolicy | undefined {
  if (text.trim() === '') {
    return undefined;
  }
  try {
    return typedPolicy('document', text, { name, key_owner: keyOwner });
  } catch {
    return undefined;
  }
}

function validate(request: TypedPolicy, signal?: AbortSignal): Promise<PolicyValidation> {
  return unwrap(
    api.POST('/policy-validation', {
      body: { document: request.document },
      // Sent as typed (typedPolicy): a key typed twice reaches the node, which says so.
      bodySerializer: () => request.body,
      signal,
    }),
  );
}

interface Answer {
  /** The body of the request it answers. */
  key: string;
  result?: PolicyValidation;
  error?: unknown;
}

/**
 * Checks a named policy, or the inline policy of a new API key, as it is
 * typed (`POST /policy-validation`): once typing has paused for
 * VALIDATION_DELAY_MS, and again whenever it changes; a check that is no
 * longer wanted is given up. Nothing is checked while `enabled` is false.
 */
export function usePolicyValidation(text: string, target: ValidationTarget, enabled: boolean): Validation {
  const { name, keyOwner } = target;
  const request = useMemo(
    () => (enabled ? requestOf(text, name, keyOwner) : undefined),
    [enabled, text, name, keyOwner],
  );
  const [answer, setAnswer] = useState<Answer | undefined>(undefined);
  const [absent, setAbsent] = useState(false);

  useEffect(() => {
    if (request === undefined || absent) {
      return undefined;
    }
    const stop = new AbortController();
    const timer = setTimeout(() => {
      validate(request, stop.signal).then(
        (result) => {
          if (!stop.signal.aborted) {
            setAnswer({ key: request.body, result });
          }
        },
        (error: unknown) => {
          if (stop.signal.aborted) {
            return;
          }
          if (isOlderNode(error)) {
            setAbsent(true);
          } else {
            setAnswer({ key: request.body, error });
          }
        },
      );
    }, VALIDATION_DELAY_MS);
    return () => {
      clearTimeout(timer);
      stop.abort();
    };
  }, [request, absent]);

  const check = useCallback(async (): Promise<PolicyValidation | undefined> => {
    if (request === undefined || absent) {
      return undefined;
    }
    if (answer?.key === request.body && answer.result !== undefined) {
      return answer.result;
    }
    try {
      const result = await validate(request);
      setAnswer({ key: request.body, result });
      return result;
    } catch (error) {
      if (isOlderNode(error)) {
        setAbsent(true);
        return undefined;
      }
      setAnswer({ key: request.body, error });
      throw error;
    }
  }, [request, absent, answer]);

  let state: ValidationState;
  if (absent) {
    state = { status: 'absent' };
  } else if (request === undefined) {
    state = { status: 'off' };
  } else if (answer?.key !== request.body) {
    state = { status: 'checking' };
  } else if (answer.result !== undefined) {
    state = { status: 'checked', result: answer.result };
  } else {
    state = { status: 'failed', error: answer.error };
  }
  return { state, check };
}

/**
 * Asks the node to check the document as it is now, and refuses to go on -
 * so that nothing is sent - when the node finds a fault in it, or cannot be
 * asked. When there is nothing the node can check, it goes on: the save is
 * checked when it is made, as before the builder.
 */
export async function requireValid(validation: Validation): Promise<void> {
  const result = await validation.check();
  if (result !== undefined && !result.valid) {
    throw new Error(NOT_SAVED_NOTICE);
  }
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
