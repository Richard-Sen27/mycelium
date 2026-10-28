import type { ButtonHTMLAttributes, ReactNode, Ref } from 'react';
import { cx } from '../cx';
import s from './Button.module.css';

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  /** "primary" fills with the accent, "warn" outlines in the warning hue (destructive), "ghost" has no border. */
  variant?: 'default' | 'primary' | 'warn' | 'ghost';
  size?: 'sm' | 'md';
  /** Shows a running marker and disables the button. */
  busy?: boolean;
  icon?: ReactNode;
  ref?: Ref<HTMLButtonElement>;
}

export function Button({
  variant = 'default',
  size = 'md',
  busy,
  icon,
  className,
  children,
  disabled,
  type = 'button',
  ref,
  ...rest
}: ButtonProps) {
  return (
    <button
      ref={ref}
      type={type}
      className={cx(s.button, s[size], className)}
      data-variant={variant}
      data-busy={busy || undefined}
      disabled={disabled || busy}
      aria-busy={busy || undefined}
      {...rest}
    >
      {icon != null && (
        <span className={s.icon} aria-hidden>
          {icon}
        </span>
      )}
      {children}
    </button>
  );
}
