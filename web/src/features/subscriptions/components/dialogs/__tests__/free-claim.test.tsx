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
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import type { ReactElement } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import type { PlanRecord } from '../../../types'
import { SubscriptionPurchaseDialog } from '../subscription-purchase-dialog'

function renderWithClient(ui: ReactElement) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  return render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>)
}

const freePlan: PlanRecord = {
  plan: {
    id: 1,
    title: 'Community daily',
    price_amount: 0,
    currency: 'USD',
    duration_unit: 'day',
    duration_value: 1,
    quota_reset_period: 'never',
    enabled: true,
    sort_order: 0,
    max_purchase_per_user: 0,
    total_amount: 500000,
    allow_balance_pay: false,
    allow_wallet_overflow: false,
    stripe_price_id: 'price_test',
  },
}

afterEach(() => vi.restoreAllMocks())

describe('Free subscription claiming', () => {
  it('allows a zero-wallet user to claim without balance or third-party payment', async () => {
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValue({ data: { success: true, data: { replayed: false } } })
    const close = vi.fn()
    renderWithClient(
      <SubscriptionPurchaseDialog
        open
        onOpenChange={close}
        plan={freePlan}
        userQuota={0}
        enableStripe
      />
    )
    expect(screen.queryByText('Insufficient balance')).not.toBeInTheDocument()
    expect(screen.queryByText('Required')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Claim for free' }))
    await waitFor(() => expect(close).toHaveBeenCalledWith(false))
    expect(post).toHaveBeenCalledWith('/api/subscription/claim', {
      plan_id: 1,
      request_key: expect.stringMatching(/^[A-Za-z0-9_-]{16,64}$/),
    })
    expect(post).toHaveBeenCalledTimes(1)
  })

  it('disables the pending claim and ignores rapid repeated clicks', async () => {
    let resolve!: (value: { data: { success: boolean } }) => void
    const post = vi.spyOn(api, 'post').mockImplementation(
      () =>
        new Promise((done) => {
          resolve = done
        })
    )
    const close = vi.fn()
    renderWithClient(
      <SubscriptionPurchaseDialog
        open
        onOpenChange={close}
        plan={freePlan}
        userQuota={0}
      />
    )
    const button = screen.getByRole('button', { name: 'Claim for free' })
    fireEvent.click(button)
    fireEvent.click(button)
    expect(screen.getByRole('button', { name: 'Claiming...' })).toBeDisabled()
    expect(post).toHaveBeenCalledTimes(1)
    resolve({ data: { success: true } })
    await waitFor(() => expect(close).toHaveBeenCalledWith(false))
  })

  it('retries an uncertain network failure using the same idempotency key', async () => {
    const post = vi
      .spyOn(api, 'post')
      .mockRejectedValueOnce(new Error('Network unavailable'))
      .mockResolvedValueOnce({ data: { success: true } })
    const close = vi.fn()
    renderWithClient(
      <SubscriptionPurchaseDialog
        open
        onOpenChange={close}
        plan={freePlan}
        userQuota={0}
      />
    )
    fireEvent.click(screen.getByRole('button', { name: 'Claim for free' }))
    await waitFor(() =>
      expect(
        screen.getByRole('button', { name: 'Claim for free' })
      ).toBeEnabled()
    )
    fireEvent.click(screen.getByRole('button', { name: 'Claim for free' }))
    await waitFor(() => expect(close).toHaveBeenCalledWith(false))
    expect(post.mock.calls[1][1]).toEqual(post.mock.calls[0][1])
  })

  it('disables claiming and explains an exhausted daily inventory', () => {
    const plan = {
      ...freePlan,
      claim: {
        daily_remaining: 0,
        daily_refresh_at: 1791561600,
        cycle_refresh_at: 1791561600,
        lifetime_count: 0,
        cycle_count: 0,
        can_claim: false,
        reason: "Today's free subscriptions are fully claimed",
      },
    }
    renderWithClient(
      <SubscriptionPurchaseDialog open onOpenChange={vi.fn()} plan={plan} />
    )
    expect(
      screen.getByRole('button', { name: 'Claim for free' })
    ).toBeDisabled()
    expect(
      screen.getByText("Today's free subscriptions are fully claimed")
    ).toBeVisible()
  })

  it('preserves the wallet balance requirement on paid plans', () => {
    const plan = {
      plan: { ...freePlan.plan, price_amount: 1, allow_balance_pay: true },
    }
    renderWithClient(
      <SubscriptionPurchaseDialog
        open
        onOpenChange={vi.fn()}
        plan={plan}
        userQuota={0}
      />
    )
    expect(screen.getByText('Insufficient balance')).toBeVisible()
    expect(
      screen.getByRole('button', { name: 'Pay with Balance' })
    ).toBeDisabled()
    expect(
      screen.queryByRole('button', { name: 'Claim for free' })
    ).not.toBeInTheDocument()
  })
  it('keeps funded paid plans on the existing balance payment endpoint', async () => {
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValue({ data: { success: true } })
    const close = vi.fn()
    const plan = {
      plan: { ...freePlan.plan, price_amount: 1, allow_balance_pay: true },
    }
    renderWithClient(
      <SubscriptionPurchaseDialog
        open
        onOpenChange={close}
        plan={plan}
        userQuota={500000}
      />
    )
    fireEvent.click(screen.getByRole('button', { name: 'Pay with Balance' }))
    await waitFor(() => expect(close).toHaveBeenCalledWith(false))
    expect(post).toHaveBeenCalledWith('/api/subscription/balance/pay', {
      plan_id: 1,
    })
  })

  it('keeps a paid Stripe checkout available when wallet balance is zero', async () => {
    const post = vi.spyOn(api, 'post').mockResolvedValue({
      data: {
        message: 'success',
        data: { pay_link: 'https://checkout.stripe.com/test-only' },
      },
    })
    const open = vi.spyOn(window, 'open').mockReturnValue(null)
    const plan = {
      plan: { ...freePlan.plan, price_amount: 1, allow_balance_pay: true },
    }
    renderWithClient(
      <SubscriptionPurchaseDialog
        open
        onOpenChange={vi.fn()}
        plan={plan}
        userQuota={0}
        enableStripe
      />
    )
    fireEvent.click(screen.getByRole('button', { name: 'Stripe' }))
    await waitFor(() =>
      expect(open).toHaveBeenCalledWith(
        'https://checkout.stripe.com/test-only',
        '_blank'
      )
    )
    expect(post).toHaveBeenCalledWith('/api/subscription/stripe/pay', {
      plan_id: 1,
    })
  })
})
