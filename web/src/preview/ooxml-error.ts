/**
 * Why a DOCX or XLSX is not shown, as far as its reader can tell:
 *
 * - `too-large`: it unpacks to more than the reader takes (OOXML_BOUNDS) -
 *   in all, in one part, by the ratio of a part, or by its count of entries;
 * - `unsafe`: a part declares a DOCTYPE or an entity, which is never read;
 * - `damaged`: it is not a zip, or not a DOCX or XLSX, or its XML is not
 *   well-formed, or it is encrypted or of a kind of zip that is not read.
 */
export type OoxmlProblem = 'too-large' | 'unsafe' | 'damaged';

/** A DOCX or XLSX that its reader refused, and why. The detail is for tests, never shown. */
export class OoxmlError extends Error {
  readonly problem: OoxmlProblem;

  constructor(problem: OoxmlProblem, detail: string) {
    super(`${problem}: ${detail}`);
    this.name = 'OoxmlError';
    this.problem = problem;
  }
}
