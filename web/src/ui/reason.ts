import { ApiError } from '../api/client';

/**
 * Why something was not done, in the words of the API: the title of its
 * problem and, where it gave one, the detail.
 */
export function reasonOf(error: unknown, otherwise = 'The upload failed'): string {
  if (error instanceof ApiError) {
    return error.problem.detail === undefined || error.problem.detail === ''
      ? error.problem.title
      : `${error.problem.title}: ${error.problem.detail}`;
  }
  return error instanceof Error ? error.message : otherwise;
}
