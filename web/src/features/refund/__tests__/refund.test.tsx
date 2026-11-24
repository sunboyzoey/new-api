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
import { cleanup, render, renderHook, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test } from 'vitest'

import { useSidebarData } from '@/hooks/use-sidebar-data'

import { Refund } from '../index'

afterEach(cleanup)

test('embeds the refund service without sharing the parent URL or allowing top navigation', () => {
  render(<Refund />)
  const frame = screen.getByTitle('Refund service')
  expect(frame).toHaveAttribute('src', 'https://weiwanga.top/refund')
  expect(frame).toHaveAttribute('referrerpolicy', 'no-referrer')
  expect(frame.getAttribute('sandbox')?.split(' ')).not.toContain(
    'allow-top-navigation'
  )
  expect(frame).toHaveClass('flex-1', 'min-h-0', 'w-full')
})

test('keeps a keyboard accessible external fallback available without waiting for the frame', async () => {
  const user = userEvent.setup()
  render(<Refund />)
  const link = screen.getByRole('link', { name: 'Open in new window' })
  expect(link).toHaveAttribute('href', 'https://weiwanga.top/refund')
  expect(link).toHaveAttribute('target', '_blank')
  expect(link).toHaveAttribute('rel', 'noopener noreferrer')
  await user.tab()
  expect(link).toHaveFocus()
  expect(
    screen.getByText(
      'If the refund page does not display or sign-in fails, open it in a new window.'
    )
  ).toBeVisible()
})

test('provides a refund menu next to the wallet for ordinary users', () => {
  const { result } = renderHook(() => useSidebarData())
  const personal = result.current.navGroups.find(
    (group) => group.id === 'personal'
  )
  expect(personal?.items).toEqual(
    expect.arrayContaining([
      expect.objectContaining({ title: 'Refund service', url: '/refund' }),
    ])
  )
})
