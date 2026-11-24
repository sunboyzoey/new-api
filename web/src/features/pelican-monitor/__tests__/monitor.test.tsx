import { webcrypto } from 'node:crypto'
import { readFileSync } from 'node:fs'

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import i18next from 'i18next'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { MonitorConfiguration } from '../config'
import { reviewedHtmlTrial } from '../html-trial-document'
import { PelicanResults } from '../results'

vi.mock('@/lib/api', () => ({
  api: { get: vi.fn(), post: vi.fn(), put: vi.fn() },
}))
const svg = '<svg xmlns="http://www.w3.org/2000/svg"><circle r="10"/></svg>'
const newest = {
  id: 22,
  target_id: 'target-A',
  group: 'group-A',
  model: 'chosen-model',
  started_at: 1700000000000,
  finished_at: 1700000001000,
  duration_ms: 1000,
  status: 'pending_review',
  error: '',
  svg,
}
const group = {
  id: 'target-A',
  group: 'group-A',
  models: ['chosen-model', 'second-model'],
  enabled: true,
  group_verified: true,
  latest: newest,
  model_results: [
    newest,
    { ...newest, model: 'second-model', status: 'failed', error: 'timeout' },
  ],
  cache: {
    requests: 40,
    errors: 3,
    cache_samples: 20,
    cache_hit_rate: 60,
    cache_read_tokens: 60,
    cache_write_tokens: 10,
    input_tokens: 100,
    output_tokens: 50,
  },
  cache_source: 'local',
  error_rate: 7.5,
  truncated: false,
}
function mockPublicMonitor<G extends { id: string }>(
  groups: G[],
  enabled = true,
  intervalMinutes = 10
) {
  vi.mocked(api.get).mockImplementation(async (url, config) => {
    if (url === '/api/pelican/groups') {
      return {
        data: {
          success: true,
          data: { groups, enabled, interval_minutes: intervalMinutes },
        },
      }
    }
    if (url === '/api/pelican/results' && !config?.params?.target) {
      return { data: { success: true, data: { items: [], has_more: false } } }
    }
    throw new Error('Unexpected monitoring endpoint or target')
  })
}

function renderQuery(children: React.ReactNode) {
  const client = new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: 0 },
      mutations: { retry: false },
    },
  })
  return render(
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  )
}
afterEach(async () => {
  useAuthStore.setState(useAuthStore.getInitialState(), true)
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  vi.resetAllMocks()
  await i18next.changeLanguage('en')
})

it.each([
  ['visitor', null],
  ['administrator', { id: 1, username: 'admin', role: ROLE.ADMIN }],
] as const)(
  'keeps the public monitor read-only for the %s',
  async (_, viewer) => {
    useAuthStore.getState().auth.setUser(viewer)
    mockPublicMonitor([group])
    const router = createRouter({
      routeTree: createRootRoute({ component: PelicanResults }),
      history: createMemoryHistory({ initialEntries: ['/'] }),
    })
    await router.load()
    renderQuery(<RouterProvider router={router} />)
    const card = within(
      await screen.findByRole('region', { name: group.group })
    )
    await card.findByText('No recent test results')
    expect(screen.queryByText('Configure group tests')).not.toBeInTheDocument()
    expect(screen.queryByLabelText('Group API key')).not.toBeInTheDocument()
    expect(vi.mocked(api.get).mock.calls.map(([url]) => url)).toEqual([
      '/api/pelican/groups',
      '/api/pelican/results',
    ])
    expect(api.get).toHaveBeenLastCalledWith(
      '/api/pelican/results',
      expect.objectContaining({ params: { before: 0, target: undefined } })
    )
  }
)

it('opens each group history by default and lets each group collapse and reopen independently', async () => {
  const user = userEvent.setup()
  const otherSVG =
    '<svg xmlns="http://www.w3.org/2000/svg"><rect width="8" height="8"/></svg>'
  vi.mocked(api.get).mockImplementation(async (url, config) => {
    if (url === '/api/pelican/groups') {
      return {
        data: {
          success: true,
          data: {
            groups: [group, { ...group, id: 'target-B', group: 'group-B' }],
            enabled: true,
            interval_minutes: 10,
          },
        },
      }
    }
    if (url !== '/api/pelican/results') {
      throw new Error('Unexpected monitoring endpoint')
    }
    expect(config?.params?.target).toBeUndefined()
    return {
      data: {
        success: true,
        data: {
          items: [
            newest,
            {
              ...newest,
              id: 32,
              target_id: 'target-B',
              group: 'group-B',
              model: 'model-B',
              svg: otherSVG,
            },
            {
              ...newest,
              id: 1,
              status: 'failed',
              error: 'timeout',
              http_status: 504,
              svg: '',
            },
            {
              ...newest,
              id: 2,
              status: 'failed',
              error: 'missing_animation',
              svg: '',
            },
          ],
          has_more: true,
        },
      },
    }
  })
  renderQuery(<PelicanResults />)
  const card = await screen.findByRole('region', { name: 'group-A' })
  const toggle = within(card).getByRole('button', {
    name: /Intelligence test.*Pelican bicycle test results/,
  })
  const collection = within(card)
  const other = within(screen.getByRole('region', { name: 'group-B' }))
  const otherToggle = other.getByRole('button', { name: /Intelligence test/ })
  expect(toggle).toHaveAttribute('aria-expanded', 'true')
  expect(otherToggle).toHaveAttribute('aria-expanded', 'true')
  expect(within(toggle).getByText('chosen-model')).toBeInTheDocument()
  expect(within(toggle).getByTitle('Drawing valid')).toHaveClass('text-success')
  expect(within(toggle).getByText('second-model')).toBeInTheDocument()
  expect(within(toggle).getByTitle('Failed')).toHaveClass('text-destructive')
  const drawing = { name: 'Pelican bicycle SVG drawing' }
  expect(await collection.findByRole('img', drawing)).toHaveAttribute(
    'src',
    `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`
  )
  expect(await other.findByRole('img', drawing)).toHaveAttribute(
    'src',
    `data:image/svg+xml;charset=utf-8,${encodeURIComponent(otherSVG)}`
  )
  expect(api.get).toHaveBeenCalledTimes(2)
  toggle.focus()
  await user.keyboard('{Enter}')
  expect(toggle).toHaveAttribute('aria-expanded', 'false')
  expect(collection.queryByRole('img', drawing)).not.toBeInTheDocument()
  expect(other.getByRole('img', drawing)).toBeInTheDocument()
  await user.click(otherToggle)
  expect(otherToggle).toHaveAttribute('aria-expanded', 'false')
  expect(other.queryByRole('img', drawing)).not.toBeInTheDocument()
  await user.click(toggle)
  expect(await collection.findByRole('img', drawing)).toBeInTheDocument()
  expect(otherToggle).toHaveAttribute('aria-expanded', 'false')
  await user.click(otherToggle)
  expect(await other.findByRole('img', drawing)).toBeInTheDocument()
  expect(screen.queryByRole('button', { name: 'Next' })).not.toBeInTheDocument()
  expect(await screen.findByText('Test timed out')).toBeInTheDocument()
  expect(
    screen.getByText('The drawing is missing the requested animation')
  ).toBeInTheDocument()
  expect(screen.getByText('HTTP 504')).toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: /run test|start test|test now/i })
  ).not.toBeInTheDocument()
  expect(api.post).not.toHaveBeenCalled()
  expect(api.put).not.toHaveBeenCalled()
})

it.each([
  [true, 19],
  [false, 20],
] as const)(
  'counts the reviewed trial within the homepage limit when recent=%s',
  async (recent, expectedImages) => {
    vi.stubGlobal('crypto', webcrypto)
    const manifest = readFileSync(
      'public/static/monitor/pelican-html-trial.json',
      'utf8'
    )
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(manifest, {
            headers: { 'content-type': 'application/json' },
          })
      )
    )
    const trialGroup = {
      ...group,
      id: reviewedHtmlTrial.target_id,
      group: reviewedHtmlTrial.group,
      models: [reviewedHtmlTrial.model],
      model_results: [],
    }
    const items = Array.from({ length: 20 }, (_, index) => ({
      ...newest,
      id: 20 - index,
      model: `result-${20 - index}`,
      started_at: reviewedHtmlTrial.finished_at + (recent ? 10 : 30) - index,
      target_id: index % 2 ? group.id : trialGroup.id,
      group: index % 2 ? group.group : trialGroup.group,
    }))
    vi.mocked(api.get).mockImplementation(async (url) => ({
      data: {
        success: true,
        data: url.endsWith('/groups')
          ? { groups: [trialGroup, group], enabled: true, interval_minutes: 10 }
          : { items, has_more: true },
      },
    }))
    renderQuery(<PelicanResults />)
    await screen.findByText('result-20')
    if (recent) await screen.findByTitle('One-time HTML drawing test')
    expect(
      screen.getAllByRole('img', { name: 'Pelican bicycle SVG drawing' })
    ).toHaveLength(expectedImages)
    if (recent) expect(screen.queryByText('result-1')).not.toBeInTheDocument()
    else {
      expect(screen.getByText('result-1')).toBeInTheDocument()
      expect(
        screen.queryByTitle('One-time HTML drawing test')
      ).not.toBeInTheDocument()
    }
  }
)

it('shows local traffic rates and remote probe rates with their own group', async () => {
  const finishedAt = 1700000000
  const local = {
    ...group,
    finished_at: finishedAt,
    trend: [
      {
        timestamp: finishedAt - 300,
        requests: 4,
        errors: 1,
        error_rate: 25,
        cache_samples: 3,
        cache_input_tokens: 100,
        cache_read_tokens: 80,
        cache_hit_rate: 80,
      },
    ],
  }
  const model = 'remote-model'
  const remote = {
    ...group,
    id: 'target-B',
    group: 'group-B',
    group_verified: false,
    models: [model],
    latest: { ...newest, target_id: 'target-B', group: 'group-B', model },
    cache_source: 'probe',
    error_rate: 25,
    cache: {
      ...group.cache,
      requests: 8,
      errors: 2,
      cache_samples: 0,
      cache_hit_rate: null,
      cache_read_tokens: 0,
      cache_write_tokens: 0,
    },
  }
  mockPublicMonitor([local, { ...remote, model_results: [remote.latest] }])
  renderQuery(<PelicanResults />)
  const localCard = within(
    await screen.findByRole('region', { name: group.group })
  )
  expect(
    localCard.getByText('Request error rate', { selector: 'dt' })
      .nextElementSibling
  ).toHaveTextContent('7.5%')
  expect(
    localCard.getByText('Cache hit rate', { selector: 'dt' }).nextElementSibling
  ).toHaveTextContent('60%')
  for (const detail of ['Requests', 'Input Tokens', 'Cache read tokens']) {
    expect(localCard.queryByText(detail)).not.toBeInTheDocument()
  }
  const history = await localCard.findByRole('figure', {
    name: 'group-A · Group status history',
  })
  expect(
    within(history).getByRole('button', { name: /Request error rate.*25%/ })
  ).toBeInTheDocument()
  expect(localCard.queryByText(model)).not.toBeInTheDocument()
  const remoteCard = within(screen.getByRole('region', { name: remote.group }))
  expect(remoteCard.getByText(model)).toBeInTheDocument()
  expect(remoteCard.queryByRole('figure')).not.toBeInTheDocument()
  expect(
    remoteCard.getByText('Test failure rate').nextElementSibling
  ).toHaveTextContent('25%')
  expect(
    remoteCard.getByText('Cache hit rate').nextElementSibling
  ).toHaveTextContent('Unknown')
})

it('shows 48 half-hour periods and lets visitors inspect weighted failures or missing observations', async () => {
  const user = userEvent.setup()
  const finishedAt = 1700000000
  const start = finishedAt - 86400
  const sample = {
    timestamp: start,
    requests: 1,
    errors: 0,
    error_rate: 0,
    cache_samples: 0,
    cache_input_tokens: 0,
    cache_read_tokens: 0,
    cache_hit_rate: null,
  }
  mockPublicMonitor([
    {
      ...group,
      cache_source: 'probe',
      finished_at: finishedAt,
      trend: [
        sample,
        { ...sample, timestamp: start + 1800, errors: 1, error_rate: 100 },
        { ...sample, timestamp: start + 2100, requests: 3 },
        { ...sample, timestamp: start + 3600, errors: 1, error_rate: 100 },
        { ...sample, timestamp: start + 5400, error_rate: null },
      ],
    },
  ])
  renderQuery(<PelicanResults />)
  const history = within(
    await screen.findByRole('figure', {
      name: 'group-A · Group status history',
    })
  )
  const periods = history.getAllByRole('button')
  expect(periods).toHaveLength(48)
  expect(periods[0]).toHaveAccessibleName(/No failures/)
  expect(periods[0]).toHaveAccessibleName(/Test failure rate.*0%/)
  expect(periods[1]).toHaveAccessibleName(/Some failures/)
  expect(periods[1]).toHaveAccessibleName(/Test failure rate.*25%/)
  expect(periods[2]).toHaveAccessibleName(/All failed/)
  expect(periods[2]).toHaveAccessibleName(/Test failure rate.*100%/)
  expect(periods[3]).toHaveAccessibleName(/No data/)
  expect(periods[47]).toHaveAccessibleName(/No data/)
  expect(periods[1]).toHaveAccessibleName(/\d{1,2}:\d{2}.+\d{1,2}:\d{2}/)

  await user.click(periods[1])
  expect(periods[1]).toHaveAttribute('aria-pressed', 'true')
  expect(history.getByText(/25%/)).toBeVisible()
  await user.keyboard('{ArrowRight}')
  expect(periods[2]).toHaveFocus()
  await user.keyboard('{Enter}')
  expect(periods[1]).toHaveAttribute('aria-pressed', 'false')
  expect(periods[2]).toHaveAttribute('aria-pressed', 'true')
  expect(history.getByText(/100%/)).toBeVisible()
})

it('keeps zero-traffic error rates and unavailable traffic data distinct from measured zero percent', async () => {
  const empty = {
    ...group,
    latest: null,
    model_results: [],
    error_rate: null,
    cache: {
      requests: 0,
      errors: 0,
      input_tokens: 0,
      output_tokens: 0,
      cache_hit_rate: null,
      cache_samples: 0,
      cache_read_tokens: 0,
      cache_write_tokens: 0,
    },
  }
  mockPublicMonitor([
    empty,
    { ...empty, id: 'offline', group: 'offline', cache_source: 'unavailable' },
  ])
  renderQuery(<PelicanResults />)
  const localCard = within(
    await screen.findByRole('region', { name: group.group })
  )
  expect(
    localCard.getByText('Request error rate').nextElementSibling
  ).toHaveTextContent('Unknown')
  expect(
    localCard.getByText('Cache hit rate').nextElementSibling
  ).toHaveTextContent('Unknown')
  const offlineCard = within(screen.getByRole('region', { name: 'offline' }))
  expect(
    offlineCard.getByText('Cache statistics unavailable')
  ).toBeInTheDocument()
  expect(offlineCard.queryByText(/^0(?:\s*%)?$/)).not.toBeInTheDocument()
})

it.each([
  [null, 'Unknown'],
  [0, '0%'],
] as const)(
  'shows %s from the server as %s for local traffic with zero logged errors',
  async (errorRate, label) => {
    mockPublicMonitor([
      {
        ...group,
        error_rate: errorRate,
        cache: { ...group.cache, requests: 40, errors: 0 },
      },
    ])
    renderQuery(<PelicanResults />)
    const card = within(
      await screen.findByRole('region', { name: group.group })
    )
    expect(
      card.getByText('Request error rate').nextElementSibling
    ).toHaveTextContent(label)
  }
)

it.each([
  [true, true, 'Scheduled every 17 minutes'],
  [false, true, 'Scheduled tests are paused'],
  [true, false, 'All monitoring groups are paused'],
] as const)(
  'reports the schedule with global enabled=%s and group enabled=%s as %s',
  async (enabled, groupEnabled, status) => {
    mockPublicMonitor([{ ...group, enabled: groupEnabled }], enabled, 17)
    renderQuery(<PelicanResults />)
    const header = within(await screen.findByRole('banner'))
    expect(header.getByText(status)).toBeInTheDocument()
    if (!enabled || !groupEnabled) {
      expect(
        header.queryByText('Scheduled every 17 minutes')
      ).not.toBeInTheDocument()
    }
  }
)

it('shows an honest empty configuration and supports traditional Chinese locale codes', async () => {
  await i18next.changeLanguage('zhTW')
  mockPublicMonitor([], false)
  const view = renderQuery(<PelicanResults />)
  expect(
    await screen.findByText('No monitoring groups configured')
  ).toBeInTheDocument()
  expect(
    screen.queryByText('Scheduled tests are paused')
  ).not.toBeInTheDocument()
  view.unmount()
  mockPublicMonitor([group])
  renderQuery(<PelicanResults />)
  expect(
    await screen.findByRole('region', { name: 'group-A' })
  ).toBeInTheDocument()
})

it('preserves a saved secret when blank and submits only explicitly configured models', async () => {
  const user = userEvent.setup()
  const config = {
    enabled: true,
    targets: [
      {
        id: 'target-A',
        group: 'group-A',
        base_url: 'https://gateway.example',
        models: ['chosen-model'],
        endpoint: 'responses',
        enabled: true,
        has_api_key: true,
      },
    ],
  }
  vi.mocked(api.get).mockResolvedValue({
    data: { success: true, data: config },
  })
  vi.mocked(api.put).mockResolvedValue({ data: { success: true } })
  renderQuery(<MonitorConfiguration />)
  const models = await screen.findByRole('textbox', {
    name: 'Test models (comma separated)',
  })
  expect(screen.getByLabelText('Group API key')).toHaveValue('')
  await user.clear(models)
  await user.type(models, 'first-explicit, second-explicit')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(api.put).toHaveBeenCalledWith(
      '/api/group-health/config',
      expect.objectContaining({
        targets: [
          expect.objectContaining({
            id: 'target-A',
            api_key: '',
            models: ['first-explicit', 'second-explicit'],
            endpoint: 'responses',
          }),
        ],
      })
    )
  )
  expect(api.post).not.toHaveBeenCalled()
})

it('saves medium reasoning effort only for the selected group', async () => {
  const user = userEvent.setup()
  const target = {
    id: 'legacy',
    group: 'legacy',
    base_url: 'https://gateway.example',
    models: ['gpt-6.2-astra'],
    endpoint: 'responses',
    enabled: true,
    has_api_key: true,
  }
  vi.mocked(api.get).mockResolvedValue({
    data: {
      success: true,
      data: {
        enabled: true,
        targets: [
          target,
          {
            ...target,
            id: 'other',
            group: 'other',
            reasoning_effort: 'medium',
          },
        ],
      },
    },
  })
  vi.mocked(api.put).mockResolvedValue({ data: { success: true } })
  renderQuery(<MonitorConfiguration />)
  const [legacy, other] = await screen.findAllByRole('switch', {
    name: 'Use medium reasoning effort',
  })
  expect(legacy).not.toBeChecked()
  expect(other).toBeChecked()
  await user.click(legacy)
  await user.click(other)
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(api.put).toHaveBeenCalledWith('/api/group-health/config', {
      enabled: true,
      targets: [
        expect.objectContaining({ id: 'legacy', reasoning_effort: 'medium' }),
        expect.objectContaining({ id: 'other', reasoning_effort: undefined }),
      ],
    })
  )
})

it('keeps the configuration editable when saving fails', async () => {
  const user = userEvent.setup()
  vi.mocked(api.get).mockResolvedValue({
    data: { success: true, data: { enabled: false, targets: [] } },
  })
  vi.mocked(api.put).mockResolvedValue({
    data: { success: false, message: 'Invalid group binding' },
  })
  renderQuery(<MonitorConfiguration />)
  await user.click(await screen.findByRole('button', { name: 'Save' }))
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Configuration could not be saved'
  )
  expect(screen.getByRole('button', { name: 'Save' })).toBeEnabled()
})
