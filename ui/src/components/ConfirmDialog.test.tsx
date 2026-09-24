// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The console's own confirmation replaces window.confirm. What is pinned:
 * the verb is on the button (never "OK"), focus lands on it so the keyboard
 * can answer, Escape and the backdrop back out, and nothing fires until the
 * operator says so.
 */

import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { ConfirmDialog } from './ConfirmDialog';

describe('ConfirmDialog', () => {
  it('names the action on its button and describes the consequence', () => {
    render(
      <ConfirmDialog title="Start a new run?" confirmLabel="Start new run" onConfirm={() => {}} onCancel={() => {}}>
        Every task executes again.
      </ConfirmDialog>,
    );
    const dialog = screen.getByRole('alertdialog', { name: 'Start a new run?' });
    expect(dialog).toHaveTextContent('Every task executes again.');
    expect(screen.getByRole('button', { name: 'Start new run' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'OK' })).toBeNull();
  });

  it('focuses the confirming button so Enter answers the question', () => {
    render(<ConfirmDialog title="Delete?" confirmLabel="Delete" onConfirm={() => {}} onCancel={() => {}} />);
    expect(screen.getByRole('button', { name: 'Delete' })).toHaveFocus();
  });

  it('fires only on confirm', () => {
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    render(<ConfirmDialog title="Retry?" confirmLabel="Retry" onConfirm={onConfirm} onCancel={onCancel} />);
    expect(onConfirm).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    expect(onConfirm).toHaveBeenCalledTimes(1);
    expect(onCancel).not.toHaveBeenCalled();
  });

  it('backs out on Cancel, on Escape, and on a backdrop click', () => {
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    render(<ConfirmDialog title="Retry?" confirmLabel="Retry" onConfirm={onConfirm} onCancel={onCancel} />);
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    fireEvent.keyDown(document, { key: 'Escape' });
    fireEvent.mouseDown(document.querySelector('.confirm-backdrop')!);
    expect(onCancel).toHaveBeenCalledTimes(3);
    expect(onConfirm).not.toHaveBeenCalled();
  });

  it('locks both buttons while the action is in flight', () => {
    render(<ConfirmDialog title="Delete?" confirmLabel="Delete" pending onConfirm={() => {}} onCancel={() => {}} />);
    expect(screen.getByRole('button', { name: 'Delete' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled();
  });

  it('returns focus to the control that opened it', () => {
    const opener = document.createElement('button');
    opener.textContent = 'open';
    document.body.appendChild(opener);
    opener.focus();
    const { unmount } = render(<ConfirmDialog title="Delete?" onConfirm={() => {}} onCancel={() => {}} />);
    expect(opener).not.toHaveFocus();
    unmount();
    expect(opener).toHaveFocus();
    opener.remove();
  });
});
