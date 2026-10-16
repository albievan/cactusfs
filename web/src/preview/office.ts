import { checkedModel } from './ooxml-check';
import type { OoxmlProblem } from './ooxml-error';
import { OOXML_TIMEOUT_MS, type OoxmlKind, type OoxmlModel } from './ooxml-model';

/** Why a DOCX or XLSX is not shown: what its reader said, or that the reader could not run or took too long. */
export type OfficeProblem = OoxmlProblem | 'load' | 'timeout';

/** What reading a DOCX or XLSX came to. */
export type OfficeOutcome = { ok: true; model: OoxmlModel } | { ok: false; problem: OfficeProblem };

/** The worker, as far as the page uses it. */
export interface OfficeWorker {
  postMessage: (message: unknown, transfer: Transferable[]) => void;
  addEventListener: (type: 'message' | 'messageerror' | 'error', listener: (event: Event) => void) => void;
  terminate: () => void;
}

/**
 * Makes the worker that reads documents: a module worker of the console's
 * own origin (`worker-src 'self'`), its own chunk, loaded only now. Vite
 * finds it by this very expression.
 */
function makeWorker(): OfficeWorker {
  return new Worker(new URL('./ooxml.worker.ts', import.meta.url), { type: 'module', name: 'ooxml' });
}

/** A document being read. */
export interface OfficeReading {
  /** What it came to; never rejects. Pending for ever once closed. */
  outcome: Promise<OfficeOutcome>;
  /** Ends the worker at once. */
  close: () => void;
}

/**
 * What a message of the worker comes to: its model when it is one of the
 * shape and size the worker makes (checkedModel), its problem when it says
 * one it may, `damaged` for anything else.
 */
function outcomeOf(data: unknown): OfficeOutcome {
  const reply = typeof data === 'object' && data !== null ? (data as { ok?: unknown; model?: unknown; problem?: unknown }) : null;
  if (reply?.ok === true) {
    const model = checkedModel(reply.model);
    return model === null ? { ok: false, problem: 'damaged' } : { ok: true, model };
  }
  if (reply?.ok === false && (reply.problem === 'too-large' || reply.problem === 'unsafe' || reply.problem === 'damaged')) {
    return { ok: false, problem: reply.problem };
  }
  return { ok: false, problem: 'damaged' };
}

/**
 * Reads a DOCX or XLSX in a worker of its own, made for it and ended once
 * it has answered, after OOXML_TIMEOUT_MS (`timeout`), or when `close` is
 * called - whichever is first. `bytes` is handed to the worker: it is
 * unusable here afterwards. A worker that cannot be made or loaded is
 * `load`; nothing is ever read on the page's own thread instead.
 */
export function readOffice(kind: OoxmlKind, bytes: ArrayBuffer, make: () => OfficeWorker = makeWorker): OfficeReading {
  let settle: (outcome: OfficeOutcome) => void = () => undefined;
  const outcome = new Promise<OfficeOutcome>((resolve) => {
    settle = resolve;
  });
  let worker: OfficeWorker;
  try {
    worker = make();
  } catch {
    settle({ ok: false, problem: 'load' });
    return { outcome, close: () => undefined };
  }
  let done = false;
  let timer: ReturnType<typeof setTimeout> | null = null;
  const end = (result: OfficeOutcome | null) => {
    if (done) {
      return;
    }
    done = true;
    if (timer !== null) {
      clearTimeout(timer);
    }
    worker.terminate();
    if (result !== null) {
      settle(result);
    }
  };
  worker.addEventListener('message', (event) => {
    const { data } = event as MessageEvent<unknown>;
    end(outcomeOf(data));
  });
  worker.addEventListener('messageerror', () => {
    end({ ok: false, problem: 'damaged' });
  });
  worker.addEventListener('error', () => {
    end({ ok: false, problem: 'load' });
  });
  timer = setTimeout(() => {
    end({ ok: false, problem: 'timeout' });
  }, OOXML_TIMEOUT_MS);
  try {
    worker.postMessage({ kind, bytes }, [bytes]);
  } catch {
    end({ ok: false, problem: 'load' });
  }
  return {
    outcome,
    close: () => {
      end(null);
    },
  };
}
