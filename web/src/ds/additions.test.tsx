import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { Chips, Countdown, Fields, fmtCount, fmtUntil, fmtUsd, gib } from '.';

describe('format', () => {
  it('formats counts, money, GiB and countdowns', () => {
    expect(fmtCount(950)).toBe('950');
    expect(fmtCount(12_300)).toBe('12.3k');
    expect(fmtCount(1000)).toBe('1k');
    expect(fmtCount(4_200_000)).toBe('4.2M');
    expect(fmtUsd(1.5)).toBe('$1.50');
    expect(fmtUsd(0.0042)).toBe('$0.0042');
    expect(gib(32 * 1024 ** 3)).toBe(32);
    const now = Date.parse('2026-10-28T12:00:00Z');
    expect(fmtUntil('2026-10-28T12:04:10Z', now)).toBe('4 m 10 s');
    expect(fmtUntil('2026-10-28T11:00:00Z', now)).toBe('expired');
    expect(fmtUntil('', now)).toBe('—');
  });
});

describe('additions', () => {
  it('Chips toggles pressed values', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(
      <Chips
        aria-label="f"
        options={[
          { value: 'a', label: 'a', count: 2 },
          { value: 'b', label: 'b' },
        ]}
        value={['a']}
        onChange={onChange}
      />,
    );
    expect(screen.getByRole('button', { name: /a/ })).toHaveAttribute('aria-pressed', 'true');
    await user.click(screen.getByRole('button', { name: 'b' }));
    expect(onChange).toHaveBeenCalledWith(['a', 'b']);
  });

  it('Countdown counts down and warns near the end', () => {
    const { container } = render(
      <>
        <Countdown to={new Date(Date.now() + 3 * 3600_000).toISOString()} warnUnderMs={60_000} />
        <Countdown to={new Date(Date.now() + 30_000).toISOString()} warnUnderMs={60_000} />
      </>,
    );
    const [far, near] = container.querySelectorAll('time');
    expect(far.textContent).toMatch(/^2 h 5\d m$/);
    expect(far.getAttribute('style')).toBeNull();
    expect(near.textContent).toMatch(/ s$/);
    expect(near.getAttribute('style')).toContain('var(--warn)');
  });

  it('Fields skips hidden rows', () => {
    render(
      <Fields
        items={[
          { label: 'A', value: '1' },
          { label: 'B', value: '2', hidden: true },
        ]}
      />,
    );
    expect(screen.getByText('A')).toBeInTheDocument();
    expect(screen.queryByText('B')).toBeNull();
  });
});
