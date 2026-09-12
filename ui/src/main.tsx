import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { BrowserRouter } from 'react-router-dom';
import { App } from './App';
import { applyTheme, getThemeChoice } from './lib/theme';
import './index.css';

// Applied BEFORE the first render so the page never paints in one theme and then
// flips to the other, which reads as a broken load.
applyTheme(getThemeChoice());

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
