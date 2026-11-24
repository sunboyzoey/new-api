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
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { ComponentProps } from 'react'
import { expect, it, vi } from 'vitest'

import { RechargeFormCard } from '../recharge-form-card'
import { ShopRechargeSection } from '../shop-recharge-section'

const link = 'https://weiwanga.top/products/relay-recharge-card'

it('shows an accessible prominent payment entry and preserves the configured destination', async () => {
  const user = userEvent.setup()
  render(<ShopRechargeSection topupLink={`${link}?source=wallet`} />)
  const entry = screen.getByRole('link', { name: 'Go to recharge payment' })
  expect(entry).toHaveAttribute('href', `${link}?source=wallet`)
  expect(entry).toHaveAttribute('target', '_blank')
  expect(entry).toHaveAttribute('rel', 'noopener noreferrer')
  await user.tab()
  expect(entry).toHaveFocus()
  expect(screen.queryByText('Get one here')).not.toBeInTheDocument()
})

it('does not render unsafe or invalid destinations', () => {
  const view = render(<ShopRechargeSection topupLink='javascript:alert(1)' />)
  expect(screen.queryByRole('link')).not.toBeInTheDocument()
  view.rerender(<ShopRechargeSection topupLink='not a URL' />)
  expect(screen.queryByRole('link')).not.toBeInTheDocument()
})

it('places the shop entry above redemption and respects disabled redemption', () => {
  const props: ComponentProps<typeof RechargeFormCard> = {
    topupInfo: null,
    presetAmounts: [],
    selectedPreset: null,
    onSelectPreset: vi.fn(),
    topupAmount: 0,
    onTopupAmountChange: vi.fn(),
    paymentAmount: 0,
    calculating: false,
    onPaymentMethodSelect: vi.fn(),
    paymentLoading: null,
    redemptionCode: '',
    onRedemptionCodeChange: vi.fn(),
    onRedeem: vi.fn(),
    redeeming: false,
    topupLink: link,
  }
  const view = render(<RechargeFormCard {...props} />)
  const entry = screen.getByRole('link', { name: 'Go to recharge payment' })
  const redeem = screen.getByRole('textbox', { name: 'Have a Code?' })
  expect(
    entry.compareDocumentPosition(redeem) & Node.DOCUMENT_POSITION_FOLLOWING
  ).toBeTruthy()
  expect(
    screen.queryByText(
      'Online topup is not enabled. Please use redemption code or contact administrator.'
    )
  ).not.toBeInTheDocument()
  view.rerender(
    <RechargeFormCard
      {...props}
      topupInfo={
        { enable_redemption: false } as ComponentProps<
          typeof RechargeFormCard
        >['topupInfo']
      }
    />
  )
  expect(
    screen.queryByRole('link', { name: 'Go to recharge payment' })
  ).not.toBeInTheDocument()
})
