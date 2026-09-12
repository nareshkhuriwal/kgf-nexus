// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Settles a rapidly-changing value before anything expensive reacts to it.
 *
 * Exists for the cron preview: the preview endpoint is the one place in the
 * console where a control fires a request on every keystroke. A half-typed
 * expression is a perfectly valid request that answers valid:false, so without
 * this the create form flashes "invalid" at the operator while they are still
 * typing the thing — which trains them to ignore the field that exists to be
 * read.
 */

import { useEffect, useState } from 'react';

export function useDebounced<T>(value: T, delayMs = 350): T {
  const [settled, setSettled] = useState(value);

  useEffect(() => {
    const timer = window.setTimeout(() => setSettled(value), delayMs);
    return () => window.clearTimeout(timer);
  }, [value, delayMs]);

  return settled;
}
