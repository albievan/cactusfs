import { useCallback, useEffect, useId, useRef } from 'react';

import { api, unwrap, type RequiredTag, type TagRules } from '../api/client';
import { useCatalogue } from '../ui/catalogue';
import { useLoad } from '../ui/hooks';
import { refused } from '../ui/sections';
import { TagRows } from '../ui/TagRows';
import { MOST_OBJECT_TAGS, suggestionsOf, TAG_RULE, type TagRow, type TagSuggestions } from '../ui/tags';

interface UploadTagsProps {
  bucket: string;
  /** The tags of the upload, as rows in the order they were typed. */
  rows: TagRow[];
  onRows: (rows: TagRow[]) => void;
  /**
   * Whether the bucket's tag rules are to be read: once files were chosen.
   * Until then the screen asks nothing more than it did.
   */
  active: boolean;
  /** Called once with the tags the rules require, when they could be read. */
  onRequired: (required: RequiredTag[]) => void;
  disabled: boolean;
}

/**
 * The tags an upload gives every file it sends (`X-CactusFS-Tagging`). Once
 * files are chosen the bucket's tag rules are read: where they can be, which
 * tags are required and with which values is said, and the rows begin with
 * the keys required; where they cannot be - the caller may not read them, or
 * the request failed - it is said that the server will tell. Either way the
 * server judges every file, and what it refuses is said beside the file.
 */
export function UploadTags({ bucket, rows, onRows, active, onRequired, disabled }: UploadTagsProps) {
  const headingId = useId();
  const hintId = useId();
  return (
    <section className="upload-tags" aria-labelledby={headingId}>
      <h3 id={headingId} className="upload-tags-heading">
        Tags
      </h3>
      {active ? (
        <WithRules bucket={bucket} rows={rows} onRows={onRows} onRequired={onRequired} disabled={disabled} hintId={hintId} />
      ) : (
        <TagRows rows={rows} onChange={onRows} label="Tags of the upload" hintId={hintId} disabled={disabled} />
      )}
      <p id={hintId} className="form-hint">
        Every file of the upload is given these tags, at most {MOST_OBJECT_TAGS}. {TAG_RULE}
      </p>
    </section>
  );
}

interface WithRulesProps {
  bucket: string;
  rows: TagRow[];
  onRows: (rows: TagRow[]) => void;
  onRequired: (required: RequiredTag[]) => void;
  disabled: boolean;
  hintId: string;
}

/** The rows, once the bucket's rules and the catalogue are asked for; and what the rules say. */
function WithRules({ bucket, rows, onRows, onRequired, disabled, hintId }: WithRulesProps) {
  const load = useCallback(
    (signal: AbortSignal) =>
      unwrap(api.GET('/buckets/{bucket}/tag-rules', { params: { path: { bucket } }, signal })),
    [bucket],
  );
  const rules = useLoad(load);
  const catalogue = useCatalogue();
  // The required keys are put in the rows once, when the rules are first read.
  const prefilled = useRef(false);
  useEffect(() => {
    if (rules.data === undefined || prefilled.current) {
      return;
    }
    prefilled.current = true;
    onRequired(rules.data.required);
  }, [rules.data, onRequired]);

  const suggestions: TagSuggestions = suggestionsOf(catalogue, rules.data?.required);
  return (
    <>
      <div className="upload-tag-rules" aria-live="polite">
        <RulesSaid rules={rules.data} loading={rules.loading} error={rules.error} />
      </div>
      <TagRows
        rows={rows}
        onChange={onRows}
        label="Tags of the upload"
        hintId={hintId}
        suggestions={suggestions}
        disabled={disabled}
      />
    </>
  );
}

/** What the rules of the bucket say an upload's tags must be, or why that is not known. */
function RulesSaid({ rules, loading, error }: { rules: TagRules | undefined; loading: boolean; error: unknown }) {
  if (rules === undefined) {
    if (loading) {
      return null;
    }
    return (
      <p className="form-hint">
        {refused(error)
          ? "You may not see the tag rules of this bucket: the server will say whether the upload's tags keep to them."
          : "The tag rules of this bucket could not be read: the server will say whether the upload's tags keep to them."}
      </p>
    );
  }
  if (!rules.lowercase && !rules.catalogue_only && rules.required.length === 0) {
    return <p className="form-hint">This bucket requires no tags.</p>;
  }
  return (
    <div className="form-hint">
      {rules.required.length === 0 ? null : (
        <>
          <p className="upload-tag-rules-title">Required by the tag rules of this bucket:</p>
          <ul className="upload-tag-rules-list">
            {rules.required.map((rule) => (
              <li key={`${rule.key}\n${rule.sub_bucket ?? ''}`}>
                <strong>{rule.key}</strong>
                {': '}
                {rule.values === undefined || rule.values.length === 0
                  ? 'any value'
                  : `one of ${rule.values.join(', ')}`}
                {rule.sub_bucket === undefined ? '' : ` (only under ${rule.sub_bucket}/)`}
              </li>
            ))}
          </ul>
        </>
      )}
      {rules.catalogue_only ? <p>Only the tags of the catalogue may be given, with the values it allows.</p> : null}
      {rules.lowercase ? <p>Tags are kept in lower case.</p> : null}
    </div>
  );
}
