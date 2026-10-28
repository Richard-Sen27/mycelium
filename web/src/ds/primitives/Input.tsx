import { useId, type InputHTMLAttributes, type ReactNode, type Ref } from 'react';
import { cx } from '../cx';
import s from './Field.module.css';

export interface InputProps extends Omit<InputHTMLAttributes<HTMLInputElement>, 'size'> {
  label?: ReactNode;
  hint?: ReactNode;
  error?: ReactNode;
  /** Monospace input for ids, keys, durations. */
  mono?: boolean;
  ref?: Ref<HTMLInputElement>;
}

/** Input is a labelled single-line text field with hint and error lines. */
export function Input({ label, hint, error, mono, className, id, ref, ...rest }: InputProps) {
  const auto = useId();
  const inputId = id ?? auto;
  const hintId = hint ? `${inputId}-hint` : undefined;
  const errId = error ? `${inputId}-err` : undefined;
  return (
    <div className={cx(s.field, className)} data-invalid={error ? true : undefined}>
      {label != null && (
        <label htmlFor={inputId} className={s.label}>
          {label}
        </label>
      )}
      <input
        ref={ref}
        id={inputId}
        className={cx(s.control, mono && s.mono)}
        aria-invalid={error ? true : undefined}
        aria-describedby={[hintId, errId].filter(Boolean).join(' ') || undefined}
        {...rest}
      />
      {hint != null && !error && (
        <div id={hintId} className={s.hint}>
          {hint}
        </div>
      )}
      {error != null && (
        <div id={errId} className={s.error} role="alert">
          {error}
        </div>
      )}
    </div>
  );
}
