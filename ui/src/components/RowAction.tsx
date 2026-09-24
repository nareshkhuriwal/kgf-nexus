// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
import { DetailDrawer } from './DetailDrawer';
import { ConfirmDialog } from './ConfirmDialog';
import { usePermission } from './Permissions';
import { useState } from 'react';
import type { ReactNode } from 'react';
export type ActionGlyph = 'edit' | 'view' | 'delete' | 'enable' | 'disable' | 'logout' | 'export';
export function ActionIcon({name}:{name:ActionGlyph}) {
 const paths:Record<ActionGlyph,ReactNode>={
 edit:<><path d="m16 3 5 5-12 12-6 1 1-6Z M14 5l5 5"/></>,
 view:<><path d="M2 12s3-7 10-7 10 7 10 7-3 7-10 7S2 12 2 12Z"/><circle cx="12" cy="12" r="3"/></>,
 delete:<><path d="M3 6h18M9 6V3h6v3M5 6l1 15h12l1-15M10 10v7M14 10v7"/></>,
 enable:<path d="m8 4 12 8-12 8Z"/>,disable:<><path d="M8 5v14M16 5v14"/></>,
 logout:<><path d="M10 3H4v18h6M9 12h12m-5-5 5 5-5 5"/></>,
 export:<><path d="M12 3v12m-5-5 5 5 5-5M4 16v5h16v-5"/></>
 };
 return <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{paths[name]}</svg>;
}
export function RowAction({label,icon,onAction,confirm,permission}:{label:string;icon:ActionGlyph;onAction:()=>unknown|Promise<unknown>;confirm?:string;permission?:string}) {
 const can=usePermission();
 const [busy,setBusy]=useState(false),[error,setError]=useState(''),[asking,setAsking]=useState(false);
 if(permission && !can(permission)) return null;
 const run=async()=>{setAsking(false);setBusy(true);setError('');try{await onAction()}catch(e){setError(e instanceof Error?e.message:'Action failed')}finally{setBusy(false)}};
 // A delete or drain is confirmed in the console's own dialog: the browser's
 // names the origin instead of the action and cannot say what is lost.
 return <><button type="button" className="icon-action" aria-label={label} title={label} disabled={busy} onClick={()=>{if(confirm)setAsking(true);else void run();}}><ActionIcon name={icon}/></button>{asking&&confirm&&<ConfirmDialog title={label} confirmLabel={label.split(' ')[0]??'Confirm'} tone={icon==='delete'?'danger':'primary'} onConfirm={()=>void run()} onCancel={()=>setAsking(false)}>{confirm}</ConfirmDialog>}{error&&<span role="alert" className="row-action-error">{error}</span>}</>;
}
export function RowDetails({label,value}:{label:string;value:unknown}) {
 const [open,setOpen]=useState(false);
 return <><button type="button" className="icon-action" aria-label={label} title={label} onClick={()=>setOpen(!open)} aria-expanded={open}><ActionIcon name="view"/></button>{open&&<DetailDrawer title={label.replace(/^View /,'')} onClose={()=>setOpen(false)}><dl className="record-facts">{Object.entries(value && typeof value==='object' ? value : {value}).map(([key,item])=><div key={key}><dt>{key.replaceAll('_',' ')}</dt><dd>{item===null || item===undefined ? <span className="faint">Not set</span> : typeof item==='object' ? <details><summary>View structured data</summary><pre>{JSON.stringify(item,null,2)}</pre></details> : typeof item==='boolean' ? (item ? 'Yes' : 'No') : String(item)}</dd></div>)}</dl><details className="record-json"><summary>Raw JSON</summary><pre>{JSON.stringify(value,null,2)}</pre></details></DetailDrawer>}</>;
}
