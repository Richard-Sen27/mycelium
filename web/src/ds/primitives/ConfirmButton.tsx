import { useState, type ReactNode } from 'react';
import { Button, type ButtonProps } from './Button';
import { Dialog } from './Dialog';
import f from './Field.module.css';

export interface ConfirmButtonProps extends Omit<ButtonProps, 'onClick' | 'title'> {
  /** Dialog title, e.g. "Cancel job_01…?" */
  confirmTitle: ReactNode;
  /** What will happen. */
  confirmBody?: ReactNode;
  /** The confirm button's label. */
  action: string;
  tone?: 'default' | 'alert';
  /** Extra content in the dialog (e.g. a note field). */
  extra?: ReactNode;
  /** Runs on confirm; a thrown error is shown in the dialog. */
  onConfirm: () => Promise<void> | void;
  /** Formats a thrown error for the dialog. */
  describeError?: (err: unknown) => string;
}

/** ConfirmButton asks in a dialog before running a destructive or network-wide action. */
export function ConfirmButton({
  confirmTitle,
  confirmBody,
  action,
  tone = 'default',
  extra,
  onConfirm,
  describeError = (e) => (e instanceof Error ? e.message : String(e)),
  children,
  ...button
}: ConfirmButtonProps) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const run = async () => {
    setBusy(true);
    setError(null);
    try {
      await onConfirm();
      setOpen(false);
    } catch (err) {
      setError(describeError(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <>
      <Button
        {...button}
        onClick={(e) => {
          e.stopPropagation();
          setError(null);
          setOpen(true);
        }}
      >
        {children}
      </Button>
      <Dialog
        open={open}
        onOpenChange={(o) => !busy && setOpen(o)}
        title={confirmTitle}
        description={confirmBody}
        tone={tone}
        footer={
          <>
            <Button variant="ghost" onClick={() => setOpen(false)} disabled={busy}>
              Back
            </Button>
            <Button variant={tone === 'alert' ? 'warn' : 'primary'} busy={busy} onClick={() => void run()}>
              {action}
            </Button>
          </>
        }
      >
        {extra != null || error ? (
          <>
            {extra}
            {error && (
              <p className={f.error} role="alert">
                {error}
              </p>
            )}
          </>
        ) : undefined}
      </Dialog>
    </>
  );
}
