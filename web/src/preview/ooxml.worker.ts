// The worker that reads a DOCX or XLSX for a preview (ADR-0028): a module
// worker of the console's own origin, made by the page for one document and
// ended by it once answered, when the preview closes, or after
// OOXML_TIMEOUT_MS. It fetches nothing and runs nothing of the document: it
// unpacks it within bounds and answers a plain model of it.

import { answer } from './ooxml';

/** What of a dedicated worker's scope this one uses. */
interface WorkerScope {
  addEventListener: (type: 'message', listener: (event: MessageEvent<unknown>) => void) => void;
  postMessage: (message: unknown, transfer: Transferable[]) => void;
}

const scope = globalThis as unknown as WorkerScope;

scope.addEventListener('message', (event) => {
  const { reply, transfer } = answer(event.data);
  scope.postMessage(reply, transfer);
});
