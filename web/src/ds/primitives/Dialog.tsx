import { Dialog as BaseDialog } from '@base-ui/react/dialog';
import type { ReactNode } from 'react';
import { cx } from '../cx';
import s from './Dialog.module.css';

export interface DialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  description?: ReactNode;
  children?: ReactNode;
  /** Buttons row at the bottom, right-aligned. */
  footer?: ReactNode;
  /** "alert" tints the frame in the warning hue (destructive confirms). */
  tone?: 'default' | 'alert';
  className?: string;
  /** Element that receives focus on open (defaults to the first tabbable). */
  initialFocus?: React.RefObject<HTMLElement | null>;
}

/** Dialog is a modal frame centred on a scrim (Base UI handles focus trap, Escape and ARIA). */
export function Dialog({
  open,
  onOpenChange,
  title,
  description,
  children,
  footer,
  tone = 'default',
  className,
  initialFocus,
}: DialogProps) {
  return (
    <BaseDialog.Root open={open} onOpenChange={(o) => onOpenChange(o)}>
      <BaseDialog.Portal>
        <BaseDialog.Backdrop className={s.backdrop} />
        <BaseDialog.Popup className={cx(s.dialog, className)} data-tone={tone} initialFocus={initialFocus}>
          <span aria-hidden className={cx(s.tick, s.tl)} />
          <span aria-hidden className={cx(s.tick, s.br)} />
          <BaseDialog.Title className={s.title}>{title}</BaseDialog.Title>
          {description != null && <BaseDialog.Description className={s.description}>{description}</BaseDialog.Description>}
          {children != null && <div className={s.body}>{children}</div>}
          {footer != null && <div className={s.footer}>{footer}</div>}
        </BaseDialog.Popup>
      </BaseDialog.Portal>
    </BaseDialog.Root>
  );
}

export interface SheetProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  children?: ReactNode;
  /** "bottom" for phones (default), "right" for the desktop inspector-style drawer. */
  side?: 'bottom' | 'right';
  className?: string;
}

/** Sheet is a dialog that slides from the bottom edge (phones) or the right edge. */
export function Sheet({ open, onOpenChange, title, children, side = 'bottom', className }: SheetProps) {
  return (
    <BaseDialog.Root open={open} onOpenChange={(o) => onOpenChange(o)}>
      <BaseDialog.Portal>
        <BaseDialog.Backdrop className={s.backdrop} />
        <BaseDialog.Popup className={cx(s.sheet, className)} data-side={side}>
          <div className={s.grip} aria-hidden />
          <div className={s.sheetHead}>
            <BaseDialog.Title className={s.title}>{title}</BaseDialog.Title>
            <BaseDialog.Close className={s.close} aria-label="Close">
              <span aria-hidden>×</span>
            </BaseDialog.Close>
          </div>
          <div className={s.sheetBody}>{children}</div>
        </BaseDialog.Popup>
      </BaseDialog.Portal>
    </BaseDialog.Root>
  );
}
