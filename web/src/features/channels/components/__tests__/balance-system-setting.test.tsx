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
import { useForm } from 'react-hook-form'
import { afterEach, assert, describe, expect, it, vi } from 'vitest'

import { Form } from '@/components/ui/form'

import {
  CHANNEL_FORM_DEFAULT_VALUES,
  buildSettingJSON,
  type ChannelFormValues,
} from '../../lib/channel-form'
import { channelSchema } from '../../types'
import { BalanceSystemSetting } from '../balance-system-setting'
import { BalanceCell } from '../channels-columns'
import { ChannelsProvider } from '../channels-provider'

function SettingsForm(props: {
  disabled?: boolean
  onSave: (setting: string) => void
}) {
  const form = useForm<ChannelFormValues>({
    defaultValues: CHANNEL_FORM_DEFAULT_VALUES,
  })
  return (
    <Form {...form}>
      <form
        onSubmit={form.handleSubmit((values) =>
          props.onSave(buildSettingJSON(values))
        )}
      >
        <BalanceSystemSetting
          control={form.control}
          disabled={props.disabled}
        />
        <button type='submit'>Save</button>
      </form>
    </Form>
  )
}

afterEach(cleanup)

describe('balance query system selection', () => {
  it('saves the selected upstream and can restore the provider default', async () => {
    const user = userEvent.setup()
    const save = vi.fn()
    render(<SettingsForm onSave={save} />)
    const select = screen.getByRole('combobox', {
      name: 'Balance query system',
    })
    expect(select).toHaveTextContent('Use channel type')
    for (const [label, value] of [
      ['New API', 'newapi'],
      ['Sub2API', 'sub2api'],
      ['Use channel type', undefined],
    ] as const) {
      await user.click(select)
      await user.click(screen.getByRole('option', { name: label }))
      expect(select).toHaveTextContent(label)
      await user.click(screen.getByRole('button', { name: 'Save' }))
      await waitFor(() => expect(save).toHaveBeenCalled())
      const saved = save.mock.lastCall
      assert(saved)
      expect(JSON.parse(saved[0]).balance_system).toBe(value)
      save.mockClear()
    }
  })
  it('supports keyboard selection', async () => {
    const user = userEvent.setup()
    render(<SettingsForm onSave={vi.fn()} />)
    screen.getByRole('combobox', { name: 'Balance query system' }).focus()
    await user.keyboard('{Enter}{ArrowDown}{Enter}')
    expect(
      screen.getByRole('combobox', { name: 'Balance query system' })
    ).toHaveTextContent('New API')
  })
  it('prevents changing locked channel settings', async () => {
    const user = userEvent.setup()
    render(<SettingsForm disabled onSave={vi.fn()} />)
    const select = screen.getByRole('combobox', {
      name: 'Balance query system',
    })
    expect(select).toBeDisabled()
    await user.click(select)
    expect(screen.queryByRole('option')).not.toBeInTheDocument()
  })
})

it.each([59, 60])(
  'shows an unlimited balance for upstream channel type %s',
  (type) => {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    const channel = channelSchema.parse({
      id: 42,
      name: 'Upstream',
      type,
      key: '',
      status: 1,
      created_time: 0,
      test_time: 0,
      response_time: 0,
      balance: -1,
      balance_updated_time: 1,
    })
    const view = render(
      <QueryClientProvider client={client}>
        <ChannelsProvider>
          <BalanceCell channel={channel} />
        </ChannelsProvider>
      </QueryClientProvider>
    )
    expect(screen.getByText('Unlimited')).toBeInTheDocument()
    expect(screen.queryByText(/-\$?1/)).not.toBeInTheDocument()
    view.unmount()
    client.clear()
  }
)

it.each([
  [38.91055428, '$39'],
  [0.12, '$0'],
  [1234567.89, '$1,234,568'],
])(
  'displays channel quota %s as an integer without abbreviations',
  (balance, expected) => {
    const client = new QueryClient()
    const channel = channelSchema.parse({
      id: 131,
      name: 'Upstream',
      type: 1,
      key: '',
      status: 1,
      created_time: 0,
      test_time: 0,
      response_time: 0,
      balance,
      used_quota: 1250000,
      balance_updated_time: 1,
    })
    const view = render(
      <QueryClientProvider client={client}>
        <ChannelsProvider>
          <BalanceCell channel={channel} />
        </ChannelsProvider>
      </QueryClientProvider>
    )
    expect(screen.getByText(expected)).toBeInTheDocument()
    expect(screen.getByText('$3')).toBeInTheDocument()
    view.unmount()
    client.clear()
  }
)
