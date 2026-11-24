/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import { RegistrationInvitesSection } from '../registration-invites-section'

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

test('administrator creates a one-time visible code and can dismiss it; invalid validity prevents creation', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: { required: true, items: [] } },
  })
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({
      data: { success: true, data: { code: 'one-time-secret' } },
    })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  const user = userEvent.setup()
  render(
    <QueryClientProvider client={client}>
      <RegistrationInvitesSection />
    </QueryClientProvider>
  )
  expect(await screen.findByText('No invitations yet')).toBeInTheDocument()
  expect(screen.getByRole('switch')).toBeChecked()
  const days = screen.getByRole('spinbutton', { name: 'Validity (days)' })
  await user.clear(days)
  await user.type(days, '31')
  expect(
    screen.getByRole('button', { name: 'Create invitation' })
  ).toBeDisabled()
  await user.clear(days)
  await user.type(days, '7')
  await user.click(screen.getByRole('button', { name: 'Create invitation' }))
  expect(await screen.findByText('one-time-secret')).toBeInTheDocument()
  expect(post).toHaveBeenCalledWith('/api/option/registration_invites', {
    expires_at: expect.any(Number),
  })
  await user.click(screen.getByRole('button', { name: 'Close' }))
  expect(screen.queryByText('one-time-secret')).not.toBeInTheDocument()
  client.clear()
})

test('revocation requires confirmation and used invitations cannot be revoked', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        required: true,
        items: [
          { id: 1, expires_at: 4000000000, used_at: 0, revoked_at: 0 },
          { id: 2, expires_at: 4000000000, used_at: 123, revoked_at: 0 },
        ],
      },
    },
  })
  const remove = vi
    .spyOn(api, 'delete')
    .mockResolvedValue({ data: { success: true } })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const user = userEvent.setup()
  render(
    <QueryClientProvider client={client}>
      <RegistrationInvitesSection />
    </QueryClientProvider>
  )
  const buttons = await screen.findAllByRole('button', { name: 'Revoke' })
  expect(buttons[1]).toBeDisabled()
  await user.click(buttons[0])
  expect(remove).not.toHaveBeenCalled()
  expect(screen.getByRole('alertdialog')).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Continue' }))
  await waitFor(() =>
    expect(remove).toHaveBeenCalledWith('/api/option/registration_invites/1')
  )
  client.clear()
})
