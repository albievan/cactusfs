// Types of notices.mjs, for vite.config.ts and the tests.

/** A production package of the console, as installed. */
export interface InstalledPackage {
  readonly name: string;
  readonly version: string;
  /** The SPDX identifier of package.json's "license", or "" when it declares none as a string. */
  readonly licence: string;
  /** Its directory, symbolic links resolved. */
  readonly dir: string;
}

/** The notices of a file shipped beside the bundle. */
export interface ShippedNotices {
  readonly what: string;
  readonly licences: string;
  /** Paths inside pdfjs-dist. */
  readonly files: readonly string[];
}

export const ALLOWED_LICENCES: readonly string[];
export const NOTICES_FILE: string;
export const PDFJS_FALLBACK_NOTICES: Readonly<Record<string, ShippedNotices>>;
export function productionPackages(root: string): InstalledPackage[];
export function licenceProblems(packages: readonly InstalledPackage[], allowed?: readonly string[]): string[];
export function noticeFiles(dir: string): string[];
export function pdfjsNoticesDirectory(version: string): string;
export function bundledPackageProblems(moduleIds: Iterable<string>, packages: readonly InstalledPackage[]): string[];
export function noticesText(root: string, fallbacks: readonly string[], allowed?: readonly string[]): string;
