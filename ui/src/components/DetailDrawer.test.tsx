// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
import { fireEvent, render, screen, cleanup } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { DetailDrawer } from './DetailDrawer';
afterEach(cleanup);
it('focuses the close button, traps tab and closes on Escape',()=>{
 const close=vi.fn();render(<DetailDrawer title="Task details" onClose={close}><button>Last action</button></DetailDrawer>);
 const first=screen.getByRole('button',{name:'Close details'}),last=screen.getByRole('button',{name:'Last action'});
 expect(first).toHaveFocus();fireEvent.keyDown(document,{key:'Tab',shiftKey:true});expect(last).toHaveFocus();
 fireEvent.keyDown(document,{key:'Tab'});expect(first).toHaveFocus();fireEvent.keyDown(document,{key:'Escape'});expect(close).toHaveBeenCalledOnce();
});
it('restores scroll and previous focus when dismissed',()=>{
 const button=document.createElement('button');document.body.append(button);button.focus();
 const before=document.body.style.overflow;const view=render(<DetailDrawer title="Details" onClose={()=>{}}>Record</DetailDrawer>);
 expect(document.body.style.overflow).toBe('hidden');view.unmount();expect(document.body.style.overflow).toBe(before);expect(button).toHaveFocus();button.remove();
});
