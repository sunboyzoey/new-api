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
import {
  createRootRoute,
  createRouter,
  createMemoryHistory,
  RouterProvider,
} from '@tanstack/react-router'
import { render, screen, waitFor, cleanup } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { GroupMonitor } from '@/features/group-monitor'
import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { ChannelMonitor } from '../index'

const channel = {
  id: 128,
  name: 'Example channel',
  status: 1,
  models: ['example-model'],
  requests: 0,
  failures: 0,
  attempts: 0,
  attempt_failures: 0,
  failure_rate: null,
  attempt_failure_rate: null,
  p95_seconds: null,
  ttft_p95_seconds: null,
  rate_limits: 0,
  timeouts: 0,
  connection_errors: 0,
  auth_errors: 0,
  upstream_errors: 0,
  output_tokens: 0,
}
const initialResponse = () => ({
  success: true,
  message: '',
  data: {
    available: true,
    started_at: '2026-09-30T00:00:00Z',
    updated_at: '2026-09-30T00:00:00Z',
    channels: [channel],
    trend: [],
    alerts: [],
    cpa: [],
  },
  evaluations: [],
  schedules: [] as {
    id: string
    channel_id: number
    model: string
    kind: string
    protocol: string
    effort: string
    enabled: boolean
    interval_minutes: number
    next_at: string
    prompt?: string
    expected_answer?: number
  }[],
  evaluation_defaults: {
    candy: 'Default candy prompt',
    pelican: 'Default pelican prompt',
  },
})
let showBusinessSamples = false
let showPublicResults = false
let response: ReturnType<typeof initialResponse>
beforeEach(() => {
  response = initialResponse()
  showPublicResults = false
  showBusinessSamples = false
  useAuthStore.getState().auth.setUser(null)
  vi.spyOn(api, 'get').mockImplementation(async (path) => {
    let data: unknown = {
      success: true,
      data: { setup: true, HeaderNavModules: '{}' },
    }
    if (String(path).includes('/group-monitor/drawings')) {
      data = {
        success: true,
        data: [
          {
            at: '2026-09-30T00:00:00Z',
            prompt: 'Historical drawing prompt',
            svg: '<svg xmlns="http://www.w3.org/2000/svg"><circle r="10"/></svg>',
          },
        ],
      }
    } else if (String(path).includes('group-monitor')) {
      data = {
        success: true,
        data: {
          available: response.data.available,
          quality_available: true,
          updated_at: '2026-09-30T00:00:00Z',
          interval_seconds: 300,
          groups: [
            {
              quality: showPublicResults
                ? [
                    {
                      model: 'example-model',
                      kind: 'candy',
                      passed: 1,
                      failed: 1,
                      errors: 1,
                      ungraded: 0,
                      history: [
                        { at: '2026-09-30T00:00:00Z', state: 'passed' },
                        { at: '2026-09-30T00:01:00Z', state: 'failed' },
                        { at: '2026-09-30T00:02:00Z', state: 'error' },
                      ],
                    },
                    {
                      model: 'example-model',
                      kind: 'pelican',
                      passed: 1,
                      failed: 0,
                      errors: 0,
                      ungraded: 0,
                      history: [
                        {
                          at: '2026-09-30T00:00:00Z',
                          state: 'passed',
                          score: 4,
                        },
                      ],
                    },
                  ]
                : [],
              average_seconds: showBusinessSamples ? 1.25 : null,
              request_history: showBusinessSamples
                ? [
                    {
                      at: 1790733600,
                      requests: 10,
                      failures: 2,
                      failure_rate: 20,
                    },
                  ]
                : [],
              name: 'Public group',
              models: ['example-model'],
              requests: showBusinessSamples ? 10 : 0,
              failures: showBusinessSamples ? 2 : 0,
              failure_rate: showBusinessSamples ? 20 : null,
              p95_seconds: showBusinessSamples ? 2 : null,
              ttft_p95_seconds: showBusinessSamples ? 0.5 : null,
            },
          ],
        },
      }
    } else if (String(path).includes('channel-monitor')) {
      data = response
    } else if (String(path).includes('/notice')) {
      data = { success: true, data: '' }
    }
    return { data } as Awaited<ReturnType<typeof api.get>>
  })
})
afterEach(() => {
  cleanup()
  useAuthStore.getState().auth.setUser(null)
  vi.restoreAllMocks()
})
async function mount(component = ChannelMonitor) {
  const query = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const i18n = createInstance()
  await i18n.init({
    lng: 'en',
    fallbackLng: 'en',
    resources: { en: { translation: {} } },
  })
  const root = createRootRoute({ component })
  const router = createRouter({
    routeTree: root,
    history: createMemoryHistory({ initialEntries: ['/'] }),
  })
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={query}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    </I18nextProvider>
  )
  return query
}
describe('Channel monitor', () => {
  it('does not request channel data for visitors', async () => {
    await mount()
    expect(api.get).not.toHaveBeenCalledWith(
      '/api/channel-monitor/admin',
      expect.anything()
    )
    expect(screen.queryByText('Example channel')).not.toBeInTheDocument()
  })
  it('shows no-data failure rates for administrators', async () => {
    useAuthStore
      .getState()
      .auth.setUser({ id: 1, username: 'test-admin', role: 100 })
    await mount()
    await screen.findAllByText('Example channel')
    expect(screen.getAllByText('No data').length).toBeGreaterThan(0)
    expect(screen.getByRole('button', { name: 'Run test' })).toBeDisabled()
    expect(screen.queryByText('0%')).not.toBeInTheDocument()
  })
  it('shows backend outage as unknown health', async () => {
    useAuthStore
      .getState()
      .auth.setUser({ id: 1, username: 'test-admin', role: 100 })
    response.data.available = false
    await mount()
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Health cannot be determined'
    )
  })
  it('requires channel and model selection plus confirmation before submitting a paid test', async () => {
    useAuthStore
      .getState()
      .auth.setUser({ id: 1, username: 'test-admin', role: 100 })
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValue({ data: { success: true } } as Awaited<
        ReturnType<typeof api.post>
      >)
    await mount()
    const user = userEvent.setup()
    const run = await screen.findByRole('button', { name: 'Run test' })
    expect(run).toBeDisabled()
    await user.selectOptions(screen.getByLabelText('Channel'), '128')
    expect(run).toBeDisabled()
    await user.selectOptions(screen.getByLabelText('Model'), 'example-model')
    await user.click(run)
    expect(post).not.toHaveBeenCalled()
    await user.click(await screen.findByRole('button', { name: 'Confirm' }))
    await waitFor(() =>
      expect(post).toHaveBeenCalledWith(
        '/api/channel-monitor/evaluations',
        expect.objectContaining({
          channel_id: 128,
          model: 'example-model',
          kind: 'candy',
        })
      )
    )
  })
})

describe('Public group monitor', () => {
  it('shows only groups and unknown health when there are no samples', async () => {
    await mount(GroupMonitor)
    await screen.findByText('Public group')
    expect(screen.getAllByText('No data').length).toBeGreaterThan(0)
    expect(screen.queryByText('Example channel')).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: 'Run test' })
    ).not.toBeInTheDocument()
    expect(api.get).not.toHaveBeenCalledWith(
      '/api/channel-monitor/admin',
      expect.anything()
    )
  })
  it('does not show an outage as healthy', async () => {
    response.data.available = false
    await mount(GroupMonitor)
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Health cannot be determined'
    )
    expect(screen.queryByText('0%')).not.toBeInTheDocument()
  })
})

it('publishes colored history and scores without test controls or detailed content', async () => {
  showPublicResults = true
  await mount(GroupMonitor)
  await screen.findByText('Public group')
  expect(
    screen.getAllByRole('img', { name: 'Test result history' })
  ).toHaveLength(2)
  expect(document.querySelectorAll('[data-result="passed"]')).toHaveLength(2)
  expect(document.querySelectorAll('[data-result="failed"]')).toHaveLength(1)
  expect(document.querySelectorAll('[data-result="error"]')).toHaveLength(1)
  expect(screen.getByText('4/5')).toBeInTheDocument()
  expect(screen.getByText('(50%)')).toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: 'Run test' })
  ).not.toBeInTheDocument()
  expect(screen.queryByRole('button', { name: 'View' })).not.toBeInTheDocument()
  expect(
    await screen.findByText('Historical drawing prompt')
  ).toBeInTheDocument()
  expect(screen.getByText('Drawing prompt')).toBeInTheDocument()
  expect(
    screen.getByText(
      'No business request samples for this group in the last hour; success and failure rates are unavailable.'
    )
  ).toBeInTheDocument()
  const preview = await screen.findByTitle('Pelican SVG preview 1')
  expect(preview).toHaveAttribute('sandbox', '')
  expect(preview).toHaveAttribute(
    'srcdoc',
    expect.stringContaining("default-src 'none'")
  )
})

it('charts real request outcomes and displays latency numbers', async () => {
  showBusinessSamples = true
  await mount(GroupMonitor)
  await screen.findByText('Public group')
  expect(
    screen.getByRole('img', {
      name: 'Request success and failure distribution',
    })
  ).toBeInTheDocument()
  expect(screen.getAllByText('80%').length).toBeGreaterThan(0)
  expect(screen.getAllByText('20%').length).toBeGreaterThan(0)
  expect(screen.getByText('2 s')).toBeInTheDocument()
  expect(screen.getByText('0.5 s')).toBeInTheDocument()
  expect(screen.getByText('1.25 s')).toBeInTheDocument()
  expect(
    screen.queryByRole('combobox', { name: 'Time window' })
  ).not.toBeInTheDocument()
  expect(
    screen.getByText('Updated every 5 minutes · fixed last-hour statistics')
  ).toBeInTheDocument()
  expect(screen.queryByText('Pending review')).not.toBeInTheDocument()
})

it('saves an edited schedule with its custom prompt, expected answer and five-minute interval', async () => {
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'test-admin', role: 100 })
  response.schedules = [
    {
      id: 'schedule-1',
      channel_id: 128,
      model: 'example-model',
      kind: 'candy',
      protocol: 'responses',
      effort: 'medium',
      enabled: false,
      interval_minutes: 60,
      next_at: '2026-09-30T00:00:00Z',
      prompt: 'Old prompt',
      expected_answer: 21,
    },
  ]
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true } } as Awaited<
      ReturnType<typeof api.post>
    >)
  await mount()
  const user = userEvent.setup()
  await user.click(await screen.findByRole('button', { name: 'Edit' }))
  expect(screen.getByLabelText('Test prompt')).toHaveValue('Old prompt')
  await user.clear(screen.getByLabelText('Test prompt'))
  await user.type(screen.getByLabelText('Test prompt'), 'Custom candy prompt')
  await user.clear(screen.getByLabelText('Expected answer'))
  await user.type(screen.getByLabelText('Expected answer'), '42')
  await user.clear(screen.getByLabelText('Schedule interval'))
  await user.type(screen.getByLabelText('Schedule interval'), '5')
  await user.click(screen.getByRole('button', { name: 'Save scheduled test' }))
  expect(post).not.toHaveBeenCalled()
  await user.click(await screen.findByRole('button', { name: 'Confirm' }))
  await waitFor(() =>
    expect(post).toHaveBeenCalledWith(
      '/api/channel-monitor/schedules',
      expect.objectContaining({
        id: 'schedule-1',
        prompt: 'Custom candy prompt',
        expected_answer: 42,
        interval_minutes: 5,
        enabled: false,
      })
    )
  )
})
it('rejects blank prompts and restores the default for the selected test type', async () => {
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'test-admin', role: 100 })
  await mount()
  const user = userEvent.setup()
  await user.selectOptions(await screen.findByLabelText('Channel'), '128')
  await user.selectOptions(screen.getByLabelText('Model'), 'example-model')
  await user.clear(screen.getByLabelText('Test prompt'))
  expect(screen.getByRole('button', { name: 'Run test' })).toBeDisabled()
  await user.click(
    screen.getByRole('button', { name: 'Restore default prompt' })
  )
  expect(screen.getByLabelText('Test prompt')).toHaveValue(
    'Default candy prompt'
  )
  await user.selectOptions(screen.getByLabelText('Test'), 'pelican')
  expect(screen.getByLabelText('Test prompt')).toHaveValue(
    'Default pelican prompt'
  )
  expect(screen.queryByLabelText('Expected answer')).not.toBeInTheDocument()
})
