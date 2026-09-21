import { useEffect, useRef, type ReactNode } from 'react';
import { createPortal } from 'react-dom';

/** A keyboard-accessible inspector shared by record details and role editing. */
export function DetailDrawer({title,onClose,children}: {title:string;onClose:()=>void;children:ReactNode}) {
 const panel=useRef<HTMLDivElement>(null);
 const close=useRef(onClose);close.current=onClose;
 useEffect(()=>{
  const previous=document.activeElement as HTMLElement | null;
  const overflow=document.body.style.overflow;document.body.style.overflow='hidden';
  panel.current?.querySelector<HTMLButtonElement>('button')?.focus();
  const keydown=(event:KeyboardEvent)=>{
   if(event.key==='Escape'){event.preventDefault();event.stopPropagation();close.current();}
   if(event.key==='Tab'){
    const elements=Array.from(panel.current?.querySelectorAll<HTMLElement>('button:not(:disabled),a[href],input:not(:disabled),select:not(:disabled),textarea:not(:disabled),summary,[tabindex="0"]') ?? []).filter(el=>!el.closest('[hidden]'));
    const first=elements[0],last=elements.at(-1);
    if(event.shiftKey && document.activeElement===first){event.preventDefault();last?.focus();}
    else if(!event.shiftKey && document.activeElement===last){event.preventDefault();first?.focus();}
   }
  };
  document.addEventListener('keydown',keydown,true);
  return ()=>{document.body.style.overflow=overflow;document.removeEventListener('keydown',keydown,true);previous?.focus();};
 },[]);
 return createPortal(<div className="inspector-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget)onClose();}}><div ref={panel} className="inspector-panel" role="dialog" aria-modal="true" aria-label={title}><header className="inspector-header"><div><span className="inspector-eyebrow">Nexus workspace</span><h2>{title}</h2></div><button type="button" aria-label="Close details" onClick={onClose}>✕</button></header><div className="inspector-content">{children}</div></div></div>,document.body);
}
