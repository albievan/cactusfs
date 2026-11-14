import { describe, expect, it } from 'vitest';

import {
  emptyRow,
  joinValues,
  requiredFor,
  rowsOf,
  splitValues,
  suggestionsOf,
  taggingOf,
  tagsOf,
  wellFormed,
  withRequired,
} from './tags';

/** A generator of numbers that is the same on every run, so that a failure can be had again. */
function seeded(seed: number): () => number {
  let state = seed >>> 0;
  return () => {
    state = (state + 0x6d2b79f5) >>> 0;
    let t = state;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/**
 * The characters of the contract (letters, digits, spaces and + - = . _ : / @),
 * those a query string gives a meaning to (& % = +), and letters beyond ASCII,
 * one of them outside the basic plane.
 */
const ALPHABET = [...'abcXYZ019 +-=._:/@&%?#', 'é', 'ß', 'Ж', '日', '😀'];

describe('tags as rows', () => {
  it('are read back in the order they were typed, a key given twice included', () => {
    const rows = [
      { id: '1', key: 'Zeta', value: 'One' },
      { id: '2', key: '', value: '' },
      { id: '3', key: 'a', value: '1' },
      { id: '4', key: 'a', value: '2' },
      { id: '5', key: '', value: 'no key' },
    ];
    // Only a row left wholly empty is no tag; an empty key is the API's to refuse.
    expect(tagsOf(rows)).toEqual([
      { key: 'Zeta', value: 'One' },
      { key: 'a', value: '1' },
      { key: 'a', value: '2' },
      { key: '', value: 'no key' },
    ]);
  });

  it('are made of tags, each a row of its own', () => {
    const rows = rowsOf([
      { key: 'b', value: '2' },
      { key: 'a', value: '1' },
    ]);
    expect(rows.map((row) => [row.key, row.value])).toEqual([
      ['b', '2'],
      ['a', '1'],
    ]);
    expect(new Set(rows.map((row) => row.id)).size).toBe(2);
    expect(emptyRow('stage')).toMatchObject({ key: 'stage', value: '' });
  });
});

describe('taggingOf', () => {
  it('makes the URL query string of X-CactusFS-Tagging, each key and value percent-encoded, in order', () => {
    const tags = [
      { key: 'stage', value: 'draft' },
      { key: 'team', value: 'a+b=c/d e' },
      { key: 'Owner', value: 'Ana@x:y' },
      { key: 'empty', value: '' },
    ];
    const header = taggingOf(tags);
    expect(header).toBe('stage=draft&team=a%2Bb%3Dc%2Fd%20e&Owner=Ana%40x%3Ay&empty=');
    // Read as a query string, it gives the tags back, in their order.
    expect([...new URLSearchParams(header)]).toEqual(tags.map((tag) => [tag.key, tag.value]));
  });

  it('is empty for no tags', () => {
    expect(taggingOf([])).toBe('');
  });

  it('reads back, as a query string, as the tags it was made of, whatever characters they hold', () => {
    const random = seeded(18);
    const text = (most: number) =>
      Array.from({ length: Math.floor(random() * (most + 1)) }, () => ALPHABET[Math.floor(random() * ALPHABET.length)] ?? 'a').join('');
    for (let round = 0; round < 500; round += 1) {
      const tags = Array.from({ length: 1 + Math.floor(random() * 10) }, () => ({ key: text(12) || 'k', value: text(20) }));
      const header = taggingOf(tags);
      // Only what a query string means nothing by is left as it is.
      expect(header, header).toMatch(/^[A-Za-z0-9\-_.!~*'()%=&]*$/);
      expect([...new URLSearchParams(header)], header).toEqual(tags.map((tag) => [tag.key, tag.value]));
    }
  });

  it('does not throw on a lone surrogate: it is sent as U+FFFD, for the API to refuse', () => {
    const lone = `a${String.fromCharCode(0xd800)}b`;
    expect(() => encodeURIComponent(lone)).toThrow();
    expect(wellFormed(lone)).toBe('a\uFFFDb');
    expect(wellFormed('😀')).toBe('😀');
    expect(taggingOf([{ key: 'k', value: lone }])).toBe('k=a%EF%BF%BDb');
    expect(taggingOf([{ key: String.fromCharCode(0xdc00), value: '' }])).toBe('%EF%BF%BD=');
  });
});

describe('values typed with commas', () => {
  it('are split at the commas, without the spaces around them or what is empty', () => {
    expect(splitValues(' draft, final ,, in review ,')).toEqual(['draft', 'final', 'in review']);
    expect(splitValues('   ')).toEqual([]);
  });

  it('are shown the way they are read', () => {
    expect(joinValues(['draft', 'final'])).toBe('draft, final');
    expect(joinValues(undefined)).toBe('');
    expect(splitValues(joinValues(['a b', 'c']))).toEqual(['a b', 'c']);
  });
});

describe('the tags the rules require of an upload', () => {
  const required = [
    { key: 'stage', values: ['draft', 'final'] },
    { key: 'period', sub_bucket: 'reports' },
    { key: 'team' },
  ];

  it('are those of the whole bucket and of the sub-bucket the key is under', () => {
    expect(requiredFor({ key: 'stage' }, 'a.txt')).toBe(true);
    expect(requiredFor({ key: 'period', sub_bucket: 'reports' }, 'reports/q1.pdf')).toBe(true);
    expect(requiredFor({ key: 'period', sub_bucket: 'reports' }, 'reportsx/q1.pdf')).toBe(false);
    expect(requiredFor({ key: 'period', sub_bucket: 'reports' }, 'a.txt')).toBe(false);
  });

  it('are added to the rows as empty rows, after them, when the rows lack them', () => {
    const rows = [{ id: '1', key: 'team', value: 'finance' }];
    const next = withRequired(rows, required, 'a.txt');
    expect(next.map((row) => [row.key, row.value])).toEqual([
      ['team', 'finance'],
      ['stage', ''],
    ]);
    expect(withRequired(rows, required, 'reports/').map((row) => row.key)).toEqual(['team', 'stage', 'period']);
    expect(withRequired([], [], 'a.txt')).toEqual([]);
  });

  it('are not added again when typed in another case', () => {
    const rows = [{ id: '1', key: 'Stage', value: 'Draft' }];
    expect(withRequired(rows, required, 'a.txt').map((row) => row.key)).toEqual(['Stage', 'team']);
  });
});

describe('suggestionsOf', () => {
  it('offers the keys and values of the rules and of the catalogue, each once', () => {
    const suggestions = suggestionsOf(
      [
        { key: 'stage', description: '', values: ['final', 'archived'], created_at: '2026-11-01T00:00:00Z' },
        { key: 'owner', description: '', values: [], created_at: '2026-11-01T00:00:00Z' },
      ],
      [{ key: 'stage', values: ['draft', 'final'] }],
    );
    expect(suggestions.keys).toEqual(['stage', 'owner']);
    expect(suggestions.valuesOf('stage')).toEqual(['draft', 'final', 'archived']);
    expect(suggestions.valuesOf('owner')).toEqual([]);
    expect(suggestions.valuesOf('nothing')).toEqual([]);
  });

  it('offers nothing when neither could be read', () => {
    expect(suggestionsOf(undefined).keys).toEqual([]);
  });
});
