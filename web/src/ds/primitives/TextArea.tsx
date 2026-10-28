import { useId, type ReactNode, type Ref, type TextareaHTMLAttributes } from 'react';
import { cx } from '../cx';
import s from './Checkbox.module.css';
import f from './Field.module.css';

export interface TextAreaProps extends TextareaHTMLAttributes<HTMLTextAreaElement> {
  label?: ReactNode;
  hint?: ReactNode;
  error?: ReactNode;
  mono?: boolean;
  ref?: Ref<HTMLTextAreaElement>;
}

/** TextArea is the multi-line sibling of Input. */
export function TextArea({ label, hint, error, mono, className, id, ref, ...rest }: TextAreaProps) {
  const auto = useId();
  const inputId = id ?? auto;
  const hintId = hint ? `${inputId}-hint` : undefined;
  const errId = error ? `${inputId}-err` : undefined;
  return (
    <div className={cx(f.field, className)} data-invalid={error ? true : undefined}>
      {label != null && (
        <label htmlFor={inputId} className={f.label}>
          {label}
        </label>
      )}
      <textarea
        ref={ref}
        id={inputId}
        className={cx(f.control, s.area, mono && f.mono)}
        aria-invalid={error ? true : undefined}
        aria-describedby={[hintId, errId].filter(Boolean).join(' ') || undefined}
        {...rest}
      />
      {hint != null && !error && (
        <div id={hintId} className={f.hint}>
          {hint}
        </div>
      )}
      {error != null && (
        <div id={errId} className={f.error} role="alert">
          {error}
        </div>
      )}
    </div>
  );
}
