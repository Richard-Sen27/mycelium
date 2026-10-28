import { Tabs as BaseTabs } from '@base-ui/react/tabs';
import type { ReactNode } from 'react';
import { cx } from '../cx';
import s from './Tabs.module.css';

export interface TabItem<V extends string = string> {
  value: V;
  label: ReactNode;
  /** A small count after the label. */
  count?: number;
  panel: ReactNode;
  disabled?: boolean;
}

export interface TabsProps<V extends string = string> {
  items: TabItem<V>[];
  value?: V;
  defaultValue?: V;
  onValueChange?: (value: V) => void;
  className?: string;
  'aria-label'?: string;
}

/** Tabs: an underlined tab strip (Base UI handles roving focus and ARIA). */
export function Tabs<V extends string = string>({
  items,
  value,
  defaultValue,
  onValueChange,
  className,
  'aria-label': ariaLabel,
}: TabsProps<V>) {
  return (
    <BaseTabs.Root
      className={cx(s.root, className)}
      value={value}
      defaultValue={defaultValue ?? items[0]?.value}
      onValueChange={(v) => onValueChange?.(v as V)}
    >
      <BaseTabs.List className={s.list} aria-label={ariaLabel}>
        {items.map((t) => (
          <BaseTabs.Tab key={t.value} value={t.value} className={s.tab} disabled={t.disabled}>
            {t.label}
            {t.count != null && <span className={s.count}>{t.count}</span>}
          </BaseTabs.Tab>
        ))}
        <BaseTabs.Indicator className={s.indicator} />
      </BaseTabs.List>
      {items.map((t) => (
        <BaseTabs.Panel key={t.value} value={t.value} className={s.panel}>
          {t.panel}
        </BaseTabs.Panel>
      ))}
    </BaseTabs.Root>
  );
}
