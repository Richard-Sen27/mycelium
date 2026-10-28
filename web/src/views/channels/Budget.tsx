import { Gauge } from '../../ds';
import type { Budget } from '../../gen/mycel/v1/envelope_pb';

export const fmtUsd = (n: number) => `$${n < 10 ? n.toFixed(2) : n.toFixed(0)}`;
export const fmtTokens = (n: number | bigint) => {
  const v = Number(n);
  if (v >= 1_000_000) return `${(v / 1_000_000).toFixed(v >= 10_000_000 ? 0 : 1)} M`;
  if (v >= 10_000) return `${Math.round(v / 1000)} k`;
  return v.toLocaleString('en-US');
};

/** hasBudget: a zero budget means unlimited. */
export function hasBudget(b: Budget | undefined): boolean {
  return !!b && (b.usd > 0 || Number(b.tokens) > 0);
}

/** remaining describes what is left of a budget ("$3.20 left", "unlimited"). */
export function remaining(budget: Budget | undefined, used: Budget | undefined): string {
  if (!hasBudget(budget)) return 'unlimited';
  if (budget!.usd > 0) return `${fmtUsd(Math.max(0, budget!.usd - (used?.usd ?? 0)))} left`;
  return `${fmtTokens(Math.max(0, Number(budget!.tokens) - Number(used?.tokens ?? 0n)))} tok left`;
}

/** BudgetGauge shows usage against a budget in USD when set, else in tokens. */
export function BudgetGauge({ budget, used, label, compact }: { budget?: Budget; used?: Budget; label?: string; compact?: boolean }) {
  if (!hasBudget(budget)) return null;
  const usd = budget!.usd > 0;
  const value = usd ? (used?.usd ?? 0) : Number(used?.tokens ?? 0n);
  const max = usd ? budget!.usd : Number(budget!.tokens);
  const text = usd ? `${fmtUsd(value)} / ${fmtUsd(max)}` : `${fmtTokens(value)} / ${fmtTokens(max)}`;
  return <Gauge value={value} max={max} label={label} valueText={text} ticks={!compact} />;
}
