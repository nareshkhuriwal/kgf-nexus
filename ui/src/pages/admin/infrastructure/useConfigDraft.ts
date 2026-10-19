// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The page's editing state for the saved platform policy.
 *
 * Holds the document as last saved (`base`) and the operator's edits
 * (`draft`) side by side, so "unsaved" is a comparison rather than a flag
 * someone has to remember to clear. Rules:
 *
 *  - A refetch never overwrites edits. When the server's document moves while
 *    the draft is clean, the draft follows it; while it is dirty, the page is
 *    told (`remoteRevision`) and the operator decides.
 *  - Save is compare-and-set on the revision the draft was loaded at; a 409
 *    is surfaced as a conflict with an explicit "load latest" choice, never
 *    as a retry that would silently overwrite someone else's change.
 *  - Validation is computed from state on every render — nothing is read
 *    back from the DOM.
 */

import { useCallback, useEffect, useMemo, useState } from 'react';
import type { UseQueryResult } from '@tanstack/react-query';
import { ApiError } from '../../../api/client';
import type { PlatformConfig } from '../../../api/platform';
import { useSavePlatformConfig } from '../../../api/hooks';
import { changedSections, fromDraft, toDraft, validateConfig, type ConfigDraft, type ConfigSection, type FieldErrors } from './model';

export interface ConfigDraftState {
  base: PlatformConfig | null;
  draft: ConfigDraft | null;
  update: (fn: (d: ConfigDraft) => ConfigDraft) => void;
  dirty: boolean;
  sections: Set<ConfigSection>;
  errors: FieldErrors;
  saving: boolean;
  saveError: unknown;
  /** A 409: someone saved a newer revision while this draft was open. */
  conflict: boolean;
  /** The server's revision when it differs from the one the draft was loaded at. */
  remoteRevision: number | null;
  notice: string;
  save: () => Promise<boolean>;
  discard: () => void;
  loadLatest: () => Promise<void>;
}

export function useConfigDraft(namespace: string, query: UseQueryResult<PlatformConfig, unknown>): ConfigDraftState {
  const [state, setState] = useState<{ base: PlatformConfig; draft: ConfigDraft } | null>(null);
  const [saveError, setSaveError] = useState<unknown>(null);
  const [notice, setNotice] = useState('');
  const mutation = useSavePlatformConfig(namespace);
  const data = query.data;

  useEffect(() => {
    if (!data) return;
    setState((s) => {
      if (!s) return { base: data, draft: toDraft(data) };
      if (JSON.stringify(s.base) === JSON.stringify(data)) return s;
      // Follow the server only while there is nothing of the operator's to lose.
      return changedSections(s.base, s.draft).size === 0 ? { base: data, draft: toDraft(data) } : s;
    });
  }, [data]);

  const sections = useMemo(() => changedSections(state?.base ?? null, state?.draft ?? null), [state]);
  const errors = useMemo(() => (state ? validateConfig(state.draft) : {}), [state]);
  const conflict = saveError instanceof ApiError && saveError.status === 409;

  const update = useCallback((fn: (d: ConfigDraft) => ConfigDraft) => {
    setState((s) => (s ? { ...s, draft: fn(s.draft) } : s));
    setNotice('');
  }, []);

  const save = useCallback(async () => {
    if (!state || Object.keys(errors).length) return false;
    setSaveError(null);
    setNotice('');
    try {
      const saved = await mutation.mutateAsync(fromDraft(state.draft));
      setState({ base: saved, draft: toDraft(saved) });
      setNotice(
        `Revision ${saved.revision} saved and audited. Registered cluster controllers pick it up on their next report; in plan mode they only report what they would change.`,
      );
      return true;
    } catch (e) {
      setSaveError(e);
      return false;
    }
  }, [state, errors, mutation]);

  const discard = useCallback(() => {
    setState((s) => (s ? { ...s, draft: toDraft(s.base) } : s));
    setSaveError(null);
    setNotice('');
  }, []);

  const loadLatest = useCallback(async () => {
    const r = await query.refetch();
    if (r.data) setState({ base: r.data, draft: toDraft(r.data) });
    setSaveError(null);
    setNotice(r.data ? `Loaded revision ${r.data.revision}. Your unsaved edits were discarded.` : '');
  }, [query]);

  return {
    base: state?.base ?? null,
    draft: state?.draft ?? null,
    update,
    dirty: sections.size > 0,
    sections,
    errors,
    saving: mutation.isPending,
    saveError,
    conflict,
    remoteRevision: data && state && data.revision !== state.base.revision ? data.revision : null,
    notice,
    save,
    discard,
    loadLatest,
  };
}
