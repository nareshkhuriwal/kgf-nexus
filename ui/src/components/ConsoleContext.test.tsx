// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, expect, it, vi } from 'vitest';
import { ConsoleContext } from './ConsoleContext';

afterEach(() => vi.restoreAllMocks());

function setup(path = '/runs/example') {
  const client = new QueryClient();
  render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[path]}><ConsoleContext /></MemoryRouter></QueryClientProvider>);
  return client;
}

it('links a nested run back to its list without exposing the opaque id as a label', () => {
  setup();
  expect(screen.getByRole('link', { name: 'Runs' })).toHaveAttribute('href', '/runs');
  expect(screen.getByText('Details')).toHaveAttribute('aria-current', 'page');
});

it('refreshes active queries and disables refresh while the browser is offline', async () => {
  const client = setup();
  const refetch = vi.spyOn(client, 'refetchQueries').mockResolvedValue();
  fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
  await waitFor(() => expect(refetch).toHaveBeenCalledWith({ type: 'active' }));
  vi.spyOn(window.navigator, 'onLine', 'get').mockReturnValue(false);
  fireEvent(window, new Event('offline'));
  expect(screen.getByText('Browser offline')).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Refresh' })).toBeDisabled();
});
