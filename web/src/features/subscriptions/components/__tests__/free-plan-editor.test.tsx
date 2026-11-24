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
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { SubscriptionsMutateDrawer } from '../subscriptions-mutate-drawer'
import { SubscriptionsProvider } from '../subscriptions-provider'

afterEach(() => vi.restoreAllMocks())

it('creates a free monthly activity with separate cycle and lifetime caps without payment compliance', async () => {
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: { success: true, data: url === '/api/group' ? ['public'] : [] },
  }))
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true } })
  const close = vi.fn()
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const user = userEvent.setup()
  render(
    <QueryClientProvider client={client}>
      <SubscriptionsProvider>
        <SubscriptionsMutateDrawer open onOpenChange={close} />
      </SubscriptionsProvider>
    </QueryClientProvider>
  )
  expect(await screen.findByLabelText('Plan Price')).toBeDisabled()
  expect(
    screen.getByText(
      'Price 0 means free. Total quota 0 means unlimited, not zero usage.'
    )
  ).toBeVisible()
  await user.type(screen.getByLabelText('Plan Title'), 'Community monthly')
  const quota = screen.getByLabelText('Quota (USD)')
  await user.clear(quota)
  await user.type(quota, '1')
  const stock = screen.getByLabelText('Daily site-wide claim limit')
  await user.clear(stock)
  await user.type(stock, '100')
  const limit = screen.getByLabelText('Per-user claims per activity cycle')
  await user.clear(limit)
  await user.type(limit, '1')
  await user.click(
    screen.getByRole('combobox', { name: 'Activity claim cycle' })
  )
  await user.click(
    await screen.findByRole('option', { name: 'Calendar month' })
  )
  expect(
    screen.getByRole('switch', {
      name: 'Block claims while this plan has an active subscription',
    })
  ).toBeChecked()
  await user.click(screen.getByRole('combobox', { name: 'Quota usage group' }))
  await user.click(await screen.findByRole('option', { name: 'public' }))
  await user.click(screen.getByRole('button', { name: 'Save changes' }))
  await waitFor(() => expect(close).toHaveBeenCalledWith(false))
  expect(post).toHaveBeenCalledWith('/api/subscription/admin/plans', {
    plan: expect.objectContaining({
      title: 'Community monthly',
      price_amount: 0,
      total_amount: 500000,
      daily_claim_limit: 100,
      claim_cycle: 'monthly',
      max_claims_per_cycle: 1,
      max_purchase_per_user: 0,
      block_active_claim: true,
      usage_group: 'public',
      upgrade_group: '',
      quota_reset_period: 'never',
    }),
  })
  client.clear()
})
