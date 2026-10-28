// A small fuzzy matcher for the command palette: the query's characters
// must appear in order; contiguous runs, word starts and an early first
// match score higher. Returns null when the query does not match.

export interface FuzzyMatch {
  score: number;
  /** Indices of matched characters in the text (for highlighting). */
  positions: number[];
}

const isBoundary = (text: string, i: number) =>
  i === 0 || /[\s\-_/:#@.<>]/.test(text[i - 1]) || (/[a-z]/.test(text[i - 1]) && /[A-Z]/.test(text[i]));

export function fuzzyMatch(query: string, text: string): FuzzyMatch | null {
  const q = query.trim().toLowerCase();
  if (q === '') return { score: 0, positions: [] };
  const t = text.toLowerCase();

  // Exact substring: best case, scored by position and boundary.
  const sub = t.indexOf(q);
  if (sub >= 0) {
    const positions = Array.from({ length: q.length }, (_, k) => sub + k);
    const score = 1000 - sub * 2 + (isBoundary(text, sub) ? 200 : 0) + (sub === 0 ? 100 : 0) - (t.length - q.length) * 0.5;
    return { score, positions };
  }

  // Subsequence (spaces in the query are optional separators).
  const qq = q.replace(/\s+/g, '');
  const positions: number[] = [];
  let score = 0;
  let ti = 0;
  let prev = -2;
  for (const ch of qq) {
    let found = -1;
    // Prefer the next word-start occurrence when it is close.
    for (let j = ti; j < t.length; j++) {
      if (t[j] === ch) {
        if (found < 0) found = j;
        if (isBoundary(text, j)) {
          if (j - found < 8) found = j;
          break;
        }
      }
    }
    if (found < 0) return null;
    score += 10;
    if (found === prev + 1) score += 15;
    if (isBoundary(text, found)) score += 20;
    score -= Math.min(found - ti, 10);
    positions.push(found);
    prev = found;
    ti = found + 1;
  }
  score -= positions[0] * 0.5;
  score -= (t.length - qq.length) * 0.2;
  return { score, positions };
}

export interface Ranked<T> {
  item: T;
  match: FuzzyMatch;
}

/** rank scores items by their best field and returns matches, best first. */
export function rank<T>(query: string, items: T[], fields: (item: T) => string[]): Ranked<T>[] {
  const out: Ranked<T>[] = [];
  for (const item of items) {
    let best: FuzzyMatch | null = null;
    const fs = fields(item);
    fs.forEach((f, i) => {
      const m = fuzzyMatch(query, f);
      if (!m) return;
      // The title (first field) wins ties over keywords; highlights only apply to it.
      const adj = i === 0 ? m : { score: m.score - 30, positions: [] };
      if (!best || adj.score > best.score) best = adj;
    });
    if (best) out.push({ item, match: best });
  }
  return out.sort((a, b) => b.match.score - a.match.score);
}
