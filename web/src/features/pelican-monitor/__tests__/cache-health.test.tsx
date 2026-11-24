import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { toast } from 'sonner'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import type { CacheTrendPoint, GroupRatePoint, GroupSummary } from '../api'
import { GroupCacheHealthPage } from '../cache-health'
import { CacheTrendCharts } from '../cache-trends'
import { aggregateGroupRates } from '../group-rate-series'

vi.mock('@/lib/api', () => ({
  api: { get: vi.fn(), post: vi.fn(), put: vi.fn() },
}))
beforeEach(() => {
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockReturnValue({
    width: 640,
    height: 320,
    top: 0,
    left: 0,
    right: 640,
    bottom: 320,
    x: 0,
    y: 0,
    toJSON: () => ({}),
  })
})
afterEach(() => {
  vi.restoreAllMocks()
  vi.resetAllMocks()
})

const finishedAt = 1700000000
const sample: CacheTrendPoint = {
  timestamp: finishedAt - 7200,
  requests: 1,
  errors: 0,
  input_tokens: 1000,
  output_tokens: 100,
  cache_input_tokens: 1000,
  cache_read_tokens: 0,
  cache_write_tokens: 0,
  cache_samples: 1,
  cache_hit_rate: 0,
}

function pendingTestRequest() {
  let resolve!: (response: { data: unknown }) => void
  const promise = new Promise<{ data: unknown }>((done) => {
    resolve = done
  })
  return { promise, resolve }
}

function mockPausedTargets(
  groups: Pick<GroupSummary, 'id' | 'group' | 'models'>[]
) {
  const summaries = groups.map((group) => ({
    ...group,
    enabled: false,
    model_results: [],
    cache_source: 'probe',
    trend: [],
    finished_at: finishedAt,
  }))
  const data: Record<string, unknown> = {
    '/api/group-health/cache': {
      groups: [],
      trends: [],
      finished_at: finishedAt,
    },
    '/api/pelican/groups': {
      groups: summaries,
      enabled: false,
      interval_minutes: 10,
    },
    '/api/pelican/results': { items: [], has_more: false },
  }
  vi.mocked(api.get).mockImplementation(async (url) => ({
    data: { success: true, data: data[url] },
  }))
}

function renderMonitor() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  })
  return render(
    <QueryClientProvider client={client}>
      <GroupCacheHealthPage />
    </QueryClientProvider>
  )
}

it('keeps local traffic distinct from a configured remote target with the same name', async () => {
  const user = userEvent.setup()
  const name = 'GPT PRO (退款流)'
  const remote = {
    id: 'remote-pro',
    group: name,
    models: ['gpt-6.2-astra'],
    model_results: [],
    cache_source: 'probe',
    finished_at: finishedAt,
    trend: [
      {
        ...sample,
        cache_read_tokens: 1000,
        cache_hit_rate: 100,
        error_rate: 0,
      },
    ],
  }
  vi.mocked(api.get).mockImplementation(async (url) => {
    const data: Record<string, unknown> = {
      '/api/group-health/cache': {
        groups: [{ ...sample, group: name }],
        channels: [{ ...sample, group: name, model: 'channel-detail-model' }],
        trends: [{ group: name, points: [sample] }],
        finished_at: finishedAt,
        truncated: false,
      },
      '/api/pelican/groups': {
        groups: [remote],
        enabled: true,
        interval_minutes: 10,
      },
      '/api/pelican/results': { items: [], has_more: false },
      '/api/group-health/config': { enabled: true, targets: [] },
    }
    return { data: { success: true, data: data[url] } }
  })
  renderMonitor()
  const selector = await screen.findByRole('combobox', { name: 'Choose Group' })
  expect(selector).toHaveValue(`${name} · Test requests`)
  expect(
    await screen.findByText('100%', { selector: 'dd' })
  ).toBeInTheDocument()
  const history = screen.getByRole('button', { name: /Intelligence test/ })
  expect(history).toHaveAttribute('aria-expanded', 'false')
  expect(api.get).not.toHaveBeenCalledWith(
    '/api/pelican/results',
    expect.anything()
  )
  await user.click(history)
  expect(api.get).toHaveBeenCalledWith(
    '/api/pelican/results',
    expect.objectContaining({
      params: { before: 0, target: remote.id },
    })
  )
  await user.click(selector)
  await user.click(
    screen.getByRole('option', { name: `${name} · Local group traffic` })
  )
  expect(
    screen.queryByRole('button', { name: 'Test now' })
  ).not.toBeInTheDocument()
  expect(screen.getByText('0%', { selector: 'dd' })).toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: /Intelligence test/ })
  ).not.toBeInTheDocument()
  expect(screen.getAllByRole('figure')).toHaveLength(1)
  for (const label of [
    'channel-detail-model',
    'Requests',
    'Errors',
    'Input Tokens',
    'Cache read tokens',
    'Token usage trend',
  ]) {
    expect(screen.queryByText(label)).not.toBeInTheDocument()
  }
  expect(api.get).not.toHaveBeenCalledWith(
    '/api/group-health/config',
    expect.anything()
  )
  await user.click(
    screen.getByRole('button', { name: 'Configure group tests' })
  )
  expect(
    await screen.findByText('Group test configuration')
  ).toBeInTheDocument()
  expect(api.post).not.toHaveBeenCalled()
  expect(api.put).not.toHaveBeenCalled()
})

it('runs the saved models of a paused target and opens its latest results for new and existing jobs', async () => {
  const user = userEvent.setup()
  const queued = vi.spyOn(toast, 'success')
  const running = vi.spyOn(toast, 'info')
  const group = {
    id: 'target+A',
    group: 'manual-A',
    models: ['model-A', 'model-B'],
  }
  mockPausedTargets([group])
  const pending = pendingTestRequest()
  vi.mocked(api.post)
    .mockReturnValueOnce(pending.promise)
    .mockResolvedValueOnce({
      data: {
        success: true,
        data: { created: false, task_id: 'existing-task' },
      },
    })
  renderMonitor()
  const button = await screen.findByRole('button', { name: 'Test now' })
  expect(screen.getByText('Current models: 2')).toBeVisible()
  await user.dblClick(button)
  expect(api.post).toHaveBeenCalledTimes(1)
  expect(api.post).toHaveBeenCalledWith(
    '/api/group-health/targets/target%2BA/test'
  )
  expect(button).toBeDisabled()
  await act(async () =>
    pending.resolve({
      data: {
        success: true,
        data: { created: true, task_id: 'existing-task' },
      },
    })
  )
  expect(
    await screen.findByRole('button', {
      name: /Intelligence test/,
      expanded: true,
    })
  ).toBeVisible()
  await waitFor(() =>
    expect(api.get).toHaveBeenCalledWith(
      '/api/pelican/results',
      expect.objectContaining({ params: { before: 0, target: group.id } })
    )
  )
  expect(queued).toHaveBeenCalledWith('Group test queued', {
    description: group.group,
  })
  await user.click(screen.getByRole('button', { name: /Intelligence test/ }))
  await user.click(button)
  expect(
    await screen.findByRole('button', {
      name: /Intelligence test/,
      expanded: true,
    })
  ).toBeVisible()
  expect(running).toHaveBeenCalledWith(
    'A test for this group is already running',
    { description: group.group }
  )
})

it('keeps the newly selected group closed when an earlier group finishes submitting', async () => {
  const user = userEvent.setup()
  mockPausedTargets(
    ['alpha', 'beta'].map((id) => ({ id, group: id, models: ['model'] }))
  )
  const pending = pendingTestRequest()
  vi.mocked(api.post).mockReturnValueOnce(pending.promise)
  renderMonitor()
  await user.click(await screen.findByRole('button', { name: 'Test now' }))
  await user.click(screen.getByRole('combobox', { name: 'Choose Group' }))
  await user.click(screen.getByRole('option', { name: 'beta · Test requests' }))
  await act(async () =>
    pending.resolve({
      data: { success: true, data: { created: true, task_id: 'alpha-task' } },
    })
  )
  expect(
    screen.getByRole('button', { name: /Intelligence test/ })
  ).toHaveAttribute('aria-expanded', 'false')
  expect(api.get).not.toHaveBeenCalledWith(
    '/api/pelican/results',
    expect.objectContaining({ params: { before: 0, target: 'beta' } })
  )
  expect(api.post).toHaveBeenCalledTimes(1)
})

it('allows retry and leaves history closed when the server rejects the manual test', async () => {
  const user = userEvent.setup()
  const queued = vi.spyOn(toast, 'success')
  mockPausedTargets([{ id: 'target', group: 'paused', models: ['model'] }])
  vi.mocked(api.post).mockResolvedValue({
    data: { success: false, message: 'Test queue unavailable' },
  })
  renderMonitor()
  const button = await screen.findByRole('button', { name: 'Test now' })
  await user.click(button)
  await waitFor(() => expect(button).toBeEnabled())
  expect(queued).not.toHaveBeenCalled()
  expect(
    screen.getByRole('button', { name: /Intelligence test/ })
  ).toHaveAttribute('aria-expanded', 'false')
  expect(api.get).not.toHaveBeenCalledWith(
    '/api/pelican/results',
    expect.anything()
  )
  await user.click(button)
  expect(api.post).toHaveBeenCalledTimes(2)
})

it('updates the displayed rate and chart together and distinguishes missing samples from a measured zero', async () => {
  const user = userEvent.setup()
  render(
    <CacheTrendCharts
      group='group-A'
      finishedAt={finishedAt}
      points={[
        {
          ...sample,
          timestamp: finishedAt - 12 * 3600,
          cache_read_tokens: 90,
          cache_input_tokens: 100,
          cache_hit_rate: 90,
        },
        { ...sample, cache_input_tokens: 900 },
        {
          ...sample,
          timestamp: finishedAt - 300,
          cache_samples: 0,
          cache_input_tokens: 0,
          cache_hit_rate: null,
        },
      ]}
    />
  )
  const rates = screen.getByRole('figure', {
    name: 'group-A · Cache hit rate trend',
  })
  expect(screen.getByRole('button', { name: 'Last 24 hours' })).toHaveAttribute(
    'aria-pressed',
    'true'
  )
  expect(screen.getByText('9%', { selector: 'dd' })).toBeInTheDocument()
  expect(within(rates).getByRole('application')).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Last 6 hours' }))
  expect(screen.getByText('0%', { selector: 'dd' })).toBeInTheDocument()
  expect(within(rates).getByRole('application')).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Last hour' }))
  expect(screen.getByText('Unknown', { selector: 'dd' })).toBeInTheDocument()
  expect(
    within(rates).getByText('No cache trend data for this period')
  ).toBeInTheDocument()
})

it('weights rate points from the report window and keeps gaps in each series independently', () => {
  const start = finishedAt - 86400
  const measured: GroupRatePoint = {
    timestamp: start,
    requests: 10,
    errors: 2,
    error_rate: 20,
    cache_read_tokens: 100,
    cache_input_tokens: 200,
    cache_samples: 1,
    cache_hit_rate: 50,
  }
  const raw = [
    measured,
    {
      ...measured,
      timestamp: start + 300,
      requests: 90,
      errors: 0,
      error_rate: 0,
      cache_read_tokens: 0,
      cache_input_tokens: 800,
      cache_hit_rate: 0,
    },
    {
      ...measured,
      timestamp: start + 1800,
      requests: 1,
      errors: 1,
      error_rate: 100,
      cache_samples: 0,
      cache_input_tokens: 0,
      cache_read_tokens: 0,
      cache_hit_rate: null,
    },
    {
      ...measured,
      timestamp: start + 5400,
      error_rate: null,
      cache_read_tokens: 0,
      cache_hit_rate: 0,
    },
    { ...measured, timestamp: finishedAt },
    { ...measured, timestamp: start - 1 },
  ]
  const day = aggregateGroupRates(raw, finishedAt, 24)
  expect(day.points).toHaveLength(48)
  expect(day.points.slice(0, 4)).toEqual([
    { timestamp: start, cache_hit_rate: 10, error_rate: 2 },
    { timestamp: start + 1800, cache_hit_rate: null, error_rate: 100 },
    { timestamp: start + 3600, cache_hit_rate: null, error_rate: null },
    { timestamp: start + 5400, cache_hit_rate: 0, error_rate: null },
  ])
  expect(day.cacheHitRate).toBeCloseTo(100 / 12)
  expect(aggregateGroupRates(raw, finishedAt, 6).points).toHaveLength(24)
  expect(aggregateGroupRates(raw, finishedAt, 1).points).toHaveLength(12)
})

it('keeps local cache trends accessible when the public monitor endpoint fails', async () => {
  vi.mocked(api.get).mockImplementation(async (url) => {
    if (url === '/api/pelican/groups') throw new Error('404')
    return {
      data: {
        success: true,
        data: {
          groups: [{ ...sample, group: 'local-only' }],
          trends: [{ group: 'local-only', points: [sample] }],
          finished_at: finishedAt,
        },
      },
    }
  })
  renderMonitor()
  expect(
    await screen.findByRole('heading', { name: 'local-only' })
  ).toBeInTheDocument()
  expect(await screen.findByRole('application')).toBeInTheDocument()
  expect(
    screen.getByText('Test results could not be loaded')
  ).toBeInTheDocument()
})

it('formats the weighted percentage for each interface language when the language changes', async () => {
  const languages = [
    ['en', '12.3%'],
    ['zhCN', '12.3%'],
    ['zhTW', '12.3%'],
    ['fr', '12,3\u00a0%'],
    ['ja', '12.3%'],
    ['ru', '12,3\u00a0%'],
    ['vi', '12,3%'],
    [
      'not_a_locale',
      new Intl.NumberFormat(undefined, {
        style: 'percent',
        maximumSignificantDigits: 3,
      }).format(0.123),
    ],
  ] as const
  const i18n = createInstance()
  await i18n.init({
    lng: 'en',
    resources: Object.fromEntries(
      languages.map(([language]) => [
        language,
        { translation: { 'Cache hit rate': 'Cache hit rate' } },
      ])
    ),
  })
  render(
    <I18nextProvider i18n={i18n}>
      <CacheTrendCharts
        group='group-A'
        finishedAt={finishedAt}
        points={[{ ...sample, cache_read_tokens: 123, cache_hit_rate: 12.3 }]}
      />
    </I18nextProvider>
  )
  const rate = screen.getByRole('definition')
  for (const [language, expected] of languages) {
    await act(async () => {
      await i18n.changeLanguage(language)
    })
    expect(rate.textContent).toBe(expected)
  }
})
