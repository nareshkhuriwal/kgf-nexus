// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The console's own confirmation dialog.
 *
 * Replaces `window.confirm`, which is the browser's dialog rather than ours:
 * it cannot be styled, it names the origin ("nexus.localhost says") instead of
 * the action, it offers "OK" for something that starts real compute, and a
 * suppressed-dialogs browser setting makes it silently return false. This one
 * says what happens, names the verb on its button, and is reachable by
 * keyboard: focus lands on the confirming button, Escape backs out, Tab stays
 * inside, and focus returns to wherever it came from on close.
 *
 * Rendered whenever it is mounted -- the caller holds the open/closed state,
 * which keeps "what is being confirmed" in the caller where the mutation is.
 */

import { useEffect, useId, useRef, type ReactNode } from 'react';
import { createPortal } from 'react-dom';

export function ConfirmDialog({
  title,
  children,
  confirmLabel = 'Confirm',
  cancelLabel = 'Cancel',
  tone = 'primary',
  pending = false,
  onConfirm,
  onCancel,
}: {
  /** The question, as a verb phrase: "Start a new run?" */
  title: string;
  /** What the action does and what it costs. Rendered as the dialog body. */
  children?: ReactNode;
  /** The verb on the confirming button. Never "OK". */
  confirmLabel?: string;
  cancelLabel?: string;
  /** danger for something that destroys or cannot be undone. */
  tone?: 'primary' | 'danger';
  /** True while the confirmed action is in flight; both buttons lock. */
  pending?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const panel = useRef<HTMLDivElement>(null);
  const cancel = useRef(onCancel);
  cancel.current = onCancel;
  const titleId = useId();
  const bodyId = useId();

  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    const overflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    panel.current?.querySelector<HTMLButtonElement>('button[data-confirm]')?.focus();
    const keydown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        event.stopPropagation();
        cancel.current();
        return;
      }
      if (event.key === 'Tab') {
        const elements = Array.from(
          panel.current?.querySelectorAll<HTMLElement>('button:not(:disabled),a[href],[tabindex="0"]') ?? [],
        );
        const first = elements[0];
        const last = elements.at(-1);
        if (event.shiftKey && document.activeElement === first) {
          event.preventDefault();
          last?.focus();
        } else if (!event.shiftKey && document.activeElement === last) {
          event.preventDefault();
          first?.focus();
        }
      }
    };
    document.addEventListener('keydown', keydown, true);
    return () => {
      document.body.style.overflow = overflow;
      document.removeEventListener('keydown', keydown, true);
      previous?.focus();
    };
  }, []);

  return createPortal(
    <div
      className="confirm-backdrop"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget && !pending) onCancel();
      }}
    >
      <div
        ref={panel}
        className={`confirm-dialog confirm-${tone}`}
        role="alertdialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={children ? bodyId : undefined}
      >
        <h2 id={titleId} className="confirm-title">
          {title}
        </h2>
        {children && (
          <div id={bodyId} className="confirm-body">
            {children}
          </div>
        )}
        <div className="confirm-actions">
          <button type="button" onClick={onCancel} disabled={pending}>
            {cancelLabel}
          </button>
          <button
            type="button"
            data-confirm
            className={tone === 'danger' ? 'danger' : 'primary'}
            onClick={onConfirm}
            disabled={pending}
          >
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>,
    document.body,
  );
}
