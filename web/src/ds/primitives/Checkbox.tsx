import { useId, type InputHTMLAttributes, type ReactNode } from 'react';
import { cx } from '../cx';
import s from './Checkbox.module.css';

export interface CheckboxProps extends Omit<InputHTMLAttributes<HTMLInputElement>, 'type' | 'onChange'> {
  label: ReactNode;
  /** A short line after the label (why it is disabled, what it means). */
  hint?: ReactNode;
  onCheckedChange?: (checked: boolean) => void;
}

/** Checkbox is a native checkbox (square hairline box, accent tick) with its label. */
export function Checkbox({ label, hint, onCheckedChange, className, id, ...rest }: CheckboxProps) {
  const auto = useId();
  const inputId = id ?? auto;
  const hintId = hint != null ? `${inputId}-hint` : undefined;
  return (
    <span className={cx(s.check, className)} data-disabled={rest.disabled || undefined}>
      <label htmlFor={inputId} className={s.labelWrap}>
        <input
          id={inputId}
          type="checkbox"
          className={s.box}
          aria-describedby={hintId}
          onChange={(e) => onCheckedChange?.(e.target.checked)}
          {...rest}
        />
        <span className={s.label}>{label}</span>
      </label>
      {hint != null && (
        <span id={hintId} className={s.hint}>
          {hint}
        </span>
      )}
    </span>
  );
}
