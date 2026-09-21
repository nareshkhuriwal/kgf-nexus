// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { BrowserRouter } from 'react-router-dom';
import { App } from './App';
import { applyTheme, getThemeChoice } from './lib/theme';
import { applyStoredAppearance } from './lib/appearance';
/**
 * The primary Inter face, imported for its URL alone.
 *
 * WHY THIS IS HERE AND NOT A <link rel="preload"> IN index.html. Vite
 * content-hashes the font filename — deliberately; see the @font-face block in
 * index.css for why a stable URL is the wrong choice for an air-gapped
 * deployment — so the real path is not knowable when index.html is written. A
 * hardcoded preload would point at a file that does not exist and the browser
 * would fetch it, fail, and then fetch the real one, which is worse than not
 * preloading at all. Importing the asset hands us the hashed URL that Vite
 * emitted.
 *
 * WHAT THE PRELOAD BUYS. Without it the font is discovered only once the
 * stylesheet has been parsed and a glyph in its unicode-range is about to be
 * painted, which puts the fetch at the end of a chain rather than at the start.
 * With `font-display: swap` that does not blank the text — it means the console
 * paints once in the fallback face and repaints in Inter a moment later, and
 * the repaint reflows every table that was measured against the wrong metrics.
 * Starting the fetch alongside the CSS usually removes the second paint.
 *
 * ONLY THE LATIN FACE. latin-ext is left to be fetched on demand: it is the
 * larger file and serves accented names that most deployments never render, so
 * preloading it would spend 85KB of the critical path on a maybe.
 */
import interLatin from './assets/fonts/inter-latin-wght-normal.woff2?url';
import './index.css';
import './console.css';

/**
 * Ask for the font as early as the module graph allows.
 *
 * Built as a real <link> rather than via new FontFace() so the browser's own
 * preload scanner and priority rules apply, and so it is inert if the face is
 * already cached. crossOrigin is required even for a same-origin font: fonts
 * are fetched in CORS mode, and a preload whose mode does not match the later
 * real request is a preload the browser discards and warns about — it would
 * then be fetched twice, which is the opposite of the point.
 */
const preload = document.createElement('link');
preload.rel = 'preload';
preload.as = 'font';
preload.type = 'font/woff2';
preload.crossOrigin = 'anonymous';
preload.href = interLatin;
document.head.appendChild(preload);

// Applied BEFORE the first render so the page never paints in one theme and then
// flips to the other, which reads as a broken load.
applyTheme(getThemeChoice());
// Same reasoning for the accent and the nav layout: applying them after the
// first paint would show the default and then correct itself.
applyStoredAppearance();

// refetchOnWindowFocus is on: an operator who tabs back to this window expects
// to see the current state, not whatever was true when they left.
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      refetchOnWindowFocus: true,
      // Data is displayed while a refetch runs, so a slow gateway never blanks
      // a panel that already has content.
      placeholderData: (prev: unknown) => prev,
    },
  },
});

const root = document.getElementById('root');
if (!root) throw new Error('missing #root element');

createRoot(root).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
);
