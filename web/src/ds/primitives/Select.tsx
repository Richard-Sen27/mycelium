import { Select as BaseSelect } from '@base-ui/react/select';
import { useId, type ReactNode } from 'react';
import { cx } from '../cx';
import s from './Field.module.css';

export interface SelectOption<V extends string = string> {
  value: V;
  label: ReactNode;
  disabled?: boolean;
}

export interface SelectProps<V extends string = string> {
  label?: ReactNode;
  options: SelectOption<V>[];
  value: V | null;
  onValueChange: (value: V) => void;
  placeholder?: ReactNode;
  disabled?: boolean;
  className?: string;
  'aria-label'?: string;
}

/** Select is Base UI's accessible select with the HUD skin. */
export function Select<V extends string = string>({
  label,
  options,
  value,
  onValueChange,
  placeholder = 'Select',
  disabled,
  className,
  'aria-label': ariaLabel,
}: SelectProps<V>) {
  const labelId = useId();
  const items = options.map((o) => ({ value: o.value, label: o.label }));
  return (
    <div className={cx(s.field, className)}>
      {label != null && (
        <span id={labelId} className={s.label}>
          {label}
        </span>
      )}
      <BaseSelect.Root<V>
        items={items}
        value={value}
        onValueChange={(v) => {
          if (v != null) onValueChange(v);
        }}
        disabled={disabled}
      >
        <BaseSelect.Trigger
          className={cx(s.control, s.trigger)}
          aria-labelledby={label != null ? labelId : undefined}
          aria-label={label == null ? ariaLabel : undefined}
        >
          <BaseSelect.Value placeholder={placeholder} />
          <BaseSelect.Icon className={s.iconSlot}>
            <span className={s.chevron} />
          </BaseSelect.Icon>
        </BaseSelect.Trigger>
        <BaseSelect.Portal>
          <BaseSelect.Positioner className={s.positioner} sideOffset={4} alignItemWithTrigger={false}>
            <BaseSelect.Popup className={s.popup}>
              <BaseSelect.List>
                {options.map((o) => (
                  <BaseSelect.Item key={o.value} value={o.value} disabled={o.disabled} className={s.item}>
                    <span className={s.indicatorSlot}>
                      <BaseSelect.ItemIndicator className={s.indicator} />
                    </span>
                    <BaseSelect.ItemText>{o.label}</BaseSelect.ItemText>
                  </BaseSelect.Item>
                ))}
              </BaseSelect.List>
            </BaseSelect.Popup>
          </BaseSelect.Positioner>
        </BaseSelect.Portal>
      </BaseSelect.Root>
    </div>
  );
}
