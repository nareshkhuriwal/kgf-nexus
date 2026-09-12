// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/// <reference types="vite/client" />

/**
 * Build-time configuration. Declared explicitly rather than relying on the
 * catch-all index signature so a typo in an env var name is a type error.
 */
interface ImportMetaEnv {
  readonly VITE_NEXUS_API_BASE?: string;
  readonly VITE_NEXUS_NAMESPACE?: string;
  readonly VITE_NEXUS_API_KEY?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
