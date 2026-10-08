import { cleanup } from '@testing-library/react';
import { afterEach, vi } from 'vitest';

// In a browser a Request for '/api/v1/…' is resolved against the address of the
// page. The Request of Node, which the tests run on, accepts absolute addresses
// only, so resolve it the way the browser does.
const NodeRequest = globalThis.Request;
class BrowserLikeRequest extends NodeRequest {
  constructor(input: RequestInfo | URL, init?: RequestInit) {
    super(typeof input === 'string' ? new URL(input, window.location.href).href : input, init);
  }
}
globalThis.Request = BrowserLikeRequest;

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  window.sessionStorage.clear();
  window.location.hash = '';
});
