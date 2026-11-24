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
import { act, cleanup, render, screen, within } from '@testing-library/react'
import i18next from 'i18next'
import type { ReactElement } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import zh from '@/i18n/locales/zh.json'
import { api } from '@/lib/api'

import { SubscriptionPlansCard } from '../subscription-plans-card'
import { WalletStatsCard } from '../wallet-stats-card'

function renderWithClient(ui: ReactElement) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  return render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>)
}

const boundary = Date.parse('2026-10-08T16:00:00Z') / 1000
const plan = {
  plan: {
    id: 1,
    title: 'Community',
    price_amount: 0,
    currency: 'USD',
    duration_unit: 'day',
    duration_value: 1,
    quota_reset_period: 'never',
    enabled: true,
    sort_order: 0,
    max_purchase_per_user: 0,
    total_amount: 500000,
  },
  claim: {
    daily_remaining: 42,
    daily_refresh_at: boundary,
    cycle_refresh_at: boundary,
    lifetime_count: 0,
    cycle_count: 0,
    can_claim: true,
  },
}

beforeEach(async () => {
  await i18next.init({
    lng: 'en',
    fallbackLng: 'en',
    resources: { en: { translation: {} } },
    interpolation: { escapeValue: false },
  })
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data: url.endsWith('/plans')
        ? [plan]
        : {
            billing_preference: 'subscription_only',
            subscriptions: [],
            all_subscriptions: [],
          },
    },
  }))
})
afterEach(async () => {
  cleanup()
  vi.useRealTimers()
  vi.restoreAllMocks()
  await i18next.changeLanguage('en')
})

it('shows remaining public rewards separately from a zero general wallet after reward plans are hidden', async () => {
  const base = {
    user_id: 1,
    plan_id: 17,
    status: 'active',
    start_time: 1,
    end_time: 4000000000,
    usage_group: 'public',
    next_reset_time: 0,
  }
  const subscriptions = [
    {
      subscription: {
        ...base,
        id: 1,
        source: 'free_registration',
        amount_total: 5000000,
        amount_used: 1000000,
      },
    },
    {
      subscription: {
        ...base,
        id: 2,
        source: 'free_checkin',
        amount_total: 2500000,
        amount_used: 500000,
      },
    },
    {
      subscription: {
        ...base,
        id: 3,
        source: 'free_checkin',
        amount_total: 2500000,
        amount_used: 0,
        status: 'cancelled',
      },
    },
    {
      subscription: {
        ...base,
        id: 4,
        source: 'free_checkin',
        amount_total: 2500000,
        amount_used: 0,
        end_time: 1,
      },
    },
    {
      subscription: {
        ...base,
        id: 5,
        source: 'admin',
        amount_total: 9000000,
        amount_used: 0,
        usage_group: '',
      },
    },
  ]
  vi.mocked(api.get).mockImplementation(async (url) => ({
    data: {
      success: true,
      data: url.endsWith('/plans')
        ? []
        : {
            billing_preference: 'subscription_first',
            subscriptions: subscriptions.slice(0, 2),
            all_subscriptions: subscriptions,
          },
    },
  }))
  renderWithClient(
    <>
      <WalletStatsCard user={null} />
      <SubscriptionPlansCard topupInfo={null} userQuota={0} />
    </>
  )
  expect(screen.getByText('General wallet balance')).toBeVisible()
  const summary = await screen.findByRole('region', {
    name: 'Public free quota',
  })
  expect(within(summary).getByText('$12')).toBeVisible()
  expect(within(summary).getByText('$8')).toBeVisible()
  expect(within(summary).getByText('$4')).toBeVisible()
  expect(
    screen.queryByRole('button', { name: 'Claim for free' })
  ).not.toBeInTheDocument()
  expect(screen.getByText(/Public registration reward/)).toBeVisible()
})

describe('Free plan inventory presentation', () => {
  it.each([
    ['en', '10/9/26, 12:00 AM'],
    ['zhCN', '2026/10/9 00:00'],
    ['zhTW', '2026/10/9 凌晨12:00'],
    ['fr', '09/10/2026 00:00'],
    ['ru', '09.10.2026, 00:00'],
    ['ja', '2026/10/09 0:00'],
    ['vi', '00:00 9/10/26'],
  ])(
    'displays Shanghai midnight inventory in language %s',
    async (language, time) => {
      await i18next.changeLanguage(language)
      renderWithClient(<SubscriptionPlansCard topupInfo={null} userQuota={0} />)
      expect(await screen.findByText('Remaining today: 42')).toBeVisible()
      expect(
        screen.getByText(`Daily refresh at ${time} (Asia/Shanghai)`)
      ).toBeVisible()
      expect(
        screen.getByRole('button', { name: 'Claim for free' })
      ).toBeEnabled()
    }
  )

  it('updates the claim label and Shanghai refresh date after switching to Chinese', async () => {
    i18next.addResourceBundle('zhCN', 'translation', zh.translation, true, true)
    renderWithClient(<SubscriptionPlansCard topupInfo={null} />)
    expect(
      await screen.findByRole('button', { name: 'Claim for free' })
    ).toBeEnabled()
    await act(async () => {
      await i18next.changeLanguage('zhCN')
    })
    expect(screen.getByRole('button', { name: '免费领取' })).toBeEnabled()
    expect(
      screen.getByText('每日库存刷新：2026/10/9 00:00（北京时间）')
    ).toBeVisible()
  })

  it('refreshes inventory when Shanghai midnight arrives', async () => {
    vi.useFakeTimers({ toFake: ['Date', 'setTimeout', 'clearTimeout'] })
    vi.setSystemTime(new Date((boundary - 1) * 1000))
    const get = vi.mocked(api.get)
    renderWithClient(<SubscriptionPlansCard topupInfo={null} />)
    await act(async () => {})
    expect(screen.getByText('Remaining today: 42')).toBeVisible()
    get.mockImplementation(async (url) => ({
      data: {
        success: true,
        data: url.endsWith('/plans')
          ? [
              {
                ...plan,
                claim: {
                  ...plan.claim,
                  daily_remaining: 100,
                  daily_refresh_at: boundary + 86400,
                  cycle_refresh_at: boundary + 86400,
                },
              },
            ]
          : {},
      },
    }))
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1251)
    })
    expect(screen.getByText('Remaining today: 100')).toBeVisible()
  })

  it('shows total, consumed, remaining quota and expiry for an existing free subscription', async () => {
    vi.spyOn(api, 'get').mockImplementation(async (url) => ({
      data: {
        success: true,
        data: url.endsWith('/plans')
          ? [plan]
          : {
              billing_preference: 'subscription_only',
              subscriptions: [],
              all_subscriptions: [
                {
                  subscription: {
                    id: 5,
                    user_id: 1,
                    plan_id: 1,
                    status: 'active',
                    source: 'free_claim',
                    start_time: 1,
                    end_time: 2,
                    amount_total: 500000,
                    amount_used: 100000,
                  },
                },
              ],
            },
      },
    }))
    renderWithClient(<SubscriptionPlansCard topupInfo={null} />)
    expect(await screen.findByText(/Community · Subscription #5/)).toBeVisible()
    expect(screen.getByText('Expired')).toBeVisible()
    expect(screen.getByText(/Expired at/)).toBeVisible()
    expect(screen.getByText(/Used Quota:/)).toHaveTextContent(
      /\$1(?:\.0+)? · Used Quota: \$0\.20* · Remaining:\s*\$0\.80*/
    )
    expect(
      screen.getAllByText('Total Quota:', { exact: false }).length
    ).toBeGreaterThan(0)
  })
})
