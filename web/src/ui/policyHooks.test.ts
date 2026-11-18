import { describe, expect, it } from 'vitest';

import type { PolicyCatalogue } from '../api/client';
import type { PolicyCatalogueView } from '../policy/model';
import { CATALOGUE_VIEW_FITS, type Fits } from './policyHooks';

// The type check of the 9.0 review: the reader's view of the catalogue is
// held to the generated type at every depth. Each line below is a check of
// the type checker (`pnpm typecheck`); a `@ts-expect-error` whose line types
// is itself an error, so a check that let drift through would fail the build.

describe('the view of the catalogue', () => {
  it('fits the generated catalogue, and drift at any depth does not', () => {
    const fits: Fits<PolicyCatalogueView, PolicyCatalogue> = true;

    // @ts-expect-error a field the contract does not have, at the top
    const top: Fits<PolicyCatalogueView & { owner?: string }, PolicyCatalogue> = true;
    // @ts-expect-error a field the contract does not have, in an object of the catalogue
    const nested: Fits<{ limits?: { max_depth: number; max_nesting?: number } }, PolicyCatalogue> = true;
    // @ts-expect-error a field the contract does not have, in the items of a list
    const listed: Fits<{ actions: readonly { name: string; owner?: string }[] }, PolicyCatalogue> = true;
    // @ts-expect-error a field of another type
    const retyped: Fits<{ limits?: { max_depth: string } }, PolicyCatalogue> = true;

    expect([CATALOGUE_VIEW_FITS, fits, top, nested, listed, retyped]).toEqual([true, true, true, true, true, true]);
  });
});
