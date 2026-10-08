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

import { useQuery, useMutation } from '@tanstack/react-query'
import {
  Activity,
  RefreshCw,
  ShieldCheck,
  FlaskConical,
  ExternalLink,
} from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { LineChart, Line, XAxis, YAxis, Tooltip, CartesianGrid } from 'recharts'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import {
  StaticDataTable,
  type StaticDataTableColumn,
} from '@/components/data-table'
import { ErrorState } from '@/components/error-state'
import { Main } from '@/components/layout'
import { LoadingState } from '@/components/loading-state'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Card,
  CardHeader,
  CardTitle,
  CardDescription,
  CardContent,
} from '@/components/ui/card'
import { ChartContainer } from '@/components/ui/chart'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { toIntlLocale } from '@/i18n/languages'
import { api } from '@/lib/api'
import { formatNumber } from '@/lib/format'
import { useAuthStore } from '@/stores/auth-store'

import { metricText, svgDocument } from './presentation'

export type Health = {
  id: number
  name: string
  status: number
  models: string[]
  requests: number
  failures: number
  attempts: number
  attempt_failures: number
  failure_rate: number | null
  attempt_failure_rate: number | null
  p95_seconds: number | null
  ttft_p95_seconds: number | null
  rate_limits: number
  timeouts: number
  connection_errors: number
  auth_errors: number
  upstream_errors: number
  output_tokens: number
}
type Evaluation = {
  prompt?: string
  expected_answer?: number
  id: string
  channel_id: number
  model: string
  kind: string
  protocol: string
  effort: string
  status: string
  created_at: string
  latency_seconds: number
  passed: boolean | null
  renderable: boolean | null
  score: number | null
  svg?: string
  answer?: string
  review?: string
  error_kind?: string
  trace_id?: string
}
type Schedule = {
  prompt?: string
  expected_answer?: number
  id: string
  channel_id: number
  model: string
  kind: string
  protocol: string
  effort: string
  enabled: boolean
  interval_minutes: number
  next_at: string
}
type MonitorResponse = {
  success: boolean
  message: string
  data: {
    available: boolean
    started_at: string
    updated_at: string
    channels: Health[]
    trend: { at: number; attempts: number; failures: number }[]
    alerts: string[]
    cpa: { route: string; outcome: string; count: number }[]
  }
  evaluations: Evaluation[]
  schedules: Schedule[]
  evaluation_defaults?: Record<string, string>
}
export function ChannelMonitor() {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.language)
  const { auth } = useAuthStore()
  const admin = (auth.user?.role ?? 0) >= 10
  const [window, setWindow] = useState('1h')
  const [channel, setChannel] = useState('')
  const [model, setModel] = useState('')
  const [kind, setKind] = useState('candy')
  const [protocol, setProtocol] = useState('responses')
  const [effort, setEffort] = useState('medium')
  const [confirm, setConfirm] = useState(false)
  const [action, setAction] = useState<'test' | 'schedule'>('test')
  const [interval, setInterval] = useState('60')
  const [promptDraft, setPromptDraft] = useState<string | null>(null)
  const [expectedAnswer, setExpectedAnswer] = useState('21')
  const [editingSchedule, setEditingSchedule] = useState<Schedule | null>(null)
  const [selected, setSelected] = useState<Evaluation | null>(null)
  const [score, setScore] = useState('3')
  const [review, setReview] = useState('')
  const query = useQuery({
    queryKey: ['channel-monitor', admin, window],
    queryFn: async () => {
      const { data } = await api.get<MonitorResponse>(
        '/api/channel-monitor/admin',
        {
          params: { window },
        }
      )
      if (!data.success) throw new Error(data.message)
      return data
    },
    enabled: admin,
    refetchInterval: 15000,
  })
  const openGrafana = async () => {
    const tab = globalThis.window.open('about:blank', '_blank')
    if (tab) tab.opener = null
    try {
      const { data } = await api.post(
        '/api/channel-monitor/grafana-session',
        {}
      )
      if (!data.success) throw new Error(data.message)
      const target = '/api/channel-monitor/grafana/d/newapi-channels'
      if (tab) tab.location.href = target
      else globalThis.window.location.assign(target)
    } catch {
      tab?.close()
      toast.error(t('Unable to open monitoring dashboard.'))
    }
  }
  const prompt = promptDraft ?? query.data?.evaluation_defaults?.[kind] ?? ''
  const validInterval =
    Number.isInteger(Number(interval)) &&
    Number(interval) >= 5 &&
    Number(interval) <= 1440
  const validAnswer =
    kind !== 'candy' ||
    (expectedAnswer.trim() !== '' &&
      Number.isInteger(Number(expectedAnswer)) &&
      Math.abs(Number(expectedAnswer)) <= 1000000000)
  const formValid =
    !!channel &&
    !!model &&
    !!prompt.trim() &&
    new TextEncoder().encode(prompt).length <= 12000 &&
    validAnswer
  const payload = {
    channel_id: Number(channel),
    model,
    kind,
    protocol,
    effort,
    prompt,
    ...(kind === 'candy' ? { expected_answer: Number(expectedAnswer) } : {}),
  }
  const submit = useMutation({
    mutationFn: async () => {
      const { data } = await api.post(
        action === 'test'
          ? '/api/channel-monitor/evaluations'
          : '/api/channel-monitor/schedules',
        action === 'test'
          ? payload
          : {
              ...payload,
              id: editingSchedule?.id ?? '',
              enabled: editingSchedule?.enabled ?? true,
              interval_minutes: Number(interval),
            }
      )
      if (!data.success) throw new Error(data.message)
    },
    onSuccess: () => {
      setConfirm(false)
      if (action === 'schedule') setEditingSchedule(null)
      toast.success(t('Saved'))
      void query.refetch()
    },
    onError: (err: Error) => toast.error(err.message),
  })
  const saveReview = useMutation({
    mutationFn: async () => {
      const { data } = await api.post(
        `/api/channel-monitor/evaluations/${selected?.id}/review`,
        {
          score: Number(score),
          review,
        }
      )
      if (!data.success) throw new Error(data.message)
    },
    onSuccess: () => {
      toast.success(t('Saved'))
      setSelected(null)
      void query.refetch()
    },
    onError: (err: Error) => toast.error(err.message),
  })
  const scheduleToggle = useMutation({
    mutationFn: async (s: Schedule) => {
      const { data } = await api.post('/api/channel-monitor/schedules', {
        ...s,
        enabled: !s.enabled,
      })
      if (!data.success) throw new Error(data.message)
    },
    onSuccess: () => {
      void query.refetch()
    },
    onError: (err: Error) => toast.error(err.message),
  })
  const data = query.data?.data
  const rows = data?.channels ?? []
  const available = data?.available ?? false
  const fmt = (v: number | null | undefined) => metricText(v, available, locale)
  const percent = (v: number | null) =>
    v == null || !available ? t('No data') : `${fmt(v)}%`
  const columns: StaticDataTableColumn<Health>[] = [
    {
      id: 'channel',
      header: t('Channel'),
      cell: (r) => (
        <div>
          <div className='font-medium'>{r.name}</div>
          <Badge variant={r.status === 1 ? 'secondary' : 'outline'}>
            {r.status === 1 ? t('Enabled') : t('Disabled')}
          </Badge>
        </div>
      ),
    },
    {
      id: 'models',
      header: t('Models'),
      cell: (r) => (
        <span className='block max-w-48 truncate' title={r.models.join(', ')}>
          {r.models.join(', ')}
        </span>
      ),
    },
    {
      id: 'requests',
      header: t('Final requests'),
      cell: (r) => fmt(r.requests),
    },
    {
      id: 'final',
      header: t('Final failure rate'),
      cell: (r) => (
        <span
          className={
            r.failure_rate && r.failure_rate >= 20 ? 'text-destructive' : ''
          }
        >
          {percent(r.failure_rate)}
        </span>
      ),
    },
    {
      id: 'attempts',
      header: t('Channel attempts'),
      cell: (r) => fmt(r.attempts),
    },
    {
      id: 'attempt',
      header: t('Attempt failure rate'),
      cell: (r) => percent(r.attempt_failure_rate),
    },
    {
      id: 'p95',
      header: t('P95 latency'),
      cell: (r) => fmt(r.p95_seconds) + (r.p95_seconds == null ? '' : ' s'),
    },
    {
      id: 'ttft',
      header: t('P95 first token'),
      cell: (r) =>
        fmt(r.ttft_p95_seconds) + (r.ttft_p95_seconds == null ? '' : ' s'),
    },
    { id: '429', header: '429', cell: (r) => fmt(r.rate_limits) },
    { id: 'timeout', header: t('Timeouts'), cell: (r) => fmt(r.timeouts) },
    {
      id: 'connect',
      header: t('Connection errors'),
      cell: (r) => fmt(r.connection_errors),
    },
    ...(admin
      ? [
          {
            id: 'auth',
            header: t('Authentication errors'),
            cell: (r: Health) => fmt(r.auth_errors),
          },
          {
            id: '5xx',
            header: '5xx',
            cell: (r: Health) => fmt(r.upstream_errors),
          },
        ]
      : []),
  ]
  const evaluationResult = (r: Evaluation) => {
    if (r.status !== 'completed') return t(r.status)
    if (r.kind === 'candy') {
      if (r.passed == null) return t('Ungraded')
      return r.passed ? t('Passed') : t('Failed')
    }
    return r.renderable ? t('SVG validated') : t('Invalid SVG')
  }
  const evalColumns: StaticDataTableColumn<Evaluation>[] = [
    {
      id: 'time',
      header: t('Time'),
      cell: (r) => new Date(r.created_at).toLocaleString(locale),
    },
    { id: 'model', header: t('Model'), cell: (r) => r.model },
    {
      id: 'kind',
      header: t('Test'),
      cell: (r) =>
        r.kind === 'candy'
          ? t('Candy logic test')
          : t('Pelican bicycle drawing'),
    },
    { id: 'result', header: t('Result'), cell: evaluationResult },
    {
      id: 'score',
      header: t('Human visual score'),
      cell: (r) => {
        if (r.kind !== 'pelican' || r.renderable !== true) return '—'
        return r.score
          ? `${formatNumber(r.score, locale)} / 5`
          : t('Not reviewed')
      },
    },
    ...(admin
      ? [
          {
            id: 'detail',
            header: t('Details'),
            cell: (r: Evaluation) => (
              <Button
                variant='ghost'
                size='sm'
                onClick={() => {
                  setSelected(r)
                  setScore(String(r.score ?? 3))
                  setReview(r.review ?? '')
                }}
              >
                {t('View')}
              </Button>
            ),
          },
        ]
      : []),
  ]
  const total = rows.reduce((acc, r) => acc + r.requests, 0)
  const failures = rows.reduce((acc, r) => acc + r.failures, 0)
  return (
    <Main className='overflow-y-auto p-4 md:p-6'>
      <div className='mx-auto flex max-w-7xl flex-col gap-6 pb-12'>
        <div className='flex flex-wrap items-center justify-between gap-4'>
          <div>
            <div className='text-muted-foreground mb-2 flex items-center gap-2 text-sm'>
              <Activity className='size-4' />
              {t('Live channel health')}
            </div>
            <h1 className='text-3xl font-semibold tracking-tight'>
              {t('Channel monitoring')}
            </h1>
            <p className='text-muted-foreground mt-2 max-w-3xl text-sm'>
              {t('Separate business reliability from model quality tests.')}
            </p>
          </div>
          <div className='flex flex-wrap gap-2'>
            <NativeSelect
              aria-label={t('Time window')}
              value={window}
              onChange={(e) => setWindow(e.target.value)}
            >
              <option value='5m'>{t('Last 5 minutes')}</option>
              <option value='1h'>{t('Last hour')}</option>
              <option value='24h'>{t('Last 24 hours')}</option>
            </NativeSelect>
            <Button
              variant='outline'
              aria-label={t('Refresh')}
              onClick={() => void query.refetch()}
              disabled={query.isFetching}
            >
              <RefreshCw className='size-4' />
            </Button>
            {admin && (
              <Button variant='outline' onClick={() => void openGrafana()}>
                <ExternalLink className='size-4' />
                Grafana
              </Button>
            )}
          </div>
        </div>
        {query.isLoading && <LoadingState />}
        {query.isError && (
          <ErrorState
            description={t('Unable to load channel monitoring.')}
            onRetry={() => void query.refetch()}
          />
        )}{' '}
        {!query.isLoading && !query.isError && (
          <>
            {!available && (
              <div
                role='alert'
                className='border-destructive/30 bg-destructive/5 rounded-xl border p-4 text-sm'
              >
                {t(
                  'Monitoring backend unavailable. Health cannot be determined.'
                )}
              </div>
            )}
            <div className='grid gap-4 sm:grid-cols-2 lg:grid-cols-4'>
              {[
                {
                  title: t('Collection status'),
                  value: available ? t('Connected') : t('Unavailable'),
                  hint: t('Refreshes every 15 seconds'),
                },
                {
                  title: t('Final requests'),
                  value: fmt(total),
                  hint: t('Estimated count in the selected window'),
                },
                {
                  title: t('Final failure rate'),
                  value: percent(total > 0 ? (failures / total) * 100 : null),
                  hint: t('Includes the final outcome after retries'),
                },
                {
                  title: t('Active channels'),
                  value: formatNumber(
                    rows.filter((r) => r.status === 1).length,
                    locale
                  ),
                  hint: t('Configured and enabled channels'),
                },
              ].map((item) => (
                <Card key={item.title}>
                  <CardHeader className='pb-2'>
                    <CardDescription>{item.title}</CardDescription>
                    <CardTitle className='text-2xl'>{item.value}</CardTitle>
                  </CardHeader>
                  <CardContent className='text-muted-foreground text-xs'>
                    {item.hint}
                  </CardContent>
                </Card>
              ))}
            </div>
            <Card>
              <CardHeader>
                <CardTitle className='flex items-center gap-2'>
                  <ShieldCheck className='size-4' />
                  {t('Channel reliability')}
                </CardTitle>
                <CardDescription>
                  {t(
                    'Attempt failures include failed retries; final failures count requests that still failed. No samples means no data.'
                  )}
                </CardDescription>
              </CardHeader>
              <CardContent className='min-w-0'>
                <StaticDataTable
                  columns={columns}
                  data={rows}
                  getRowKey={(r) => r.id}
                  emptyContent={t('No channels')}
                />
              </CardContent>
            </Card>
            <div className='grid gap-4 lg:grid-cols-3'>
              <Card className='min-w-0 lg:col-span-2'>
                <CardHeader>
                  <CardTitle>{t('Request trend')}</CardTitle>
                  <CardDescription>
                    {t('Channel attempts and failures per minute')}
                  </CardDescription>
                </CardHeader>
                <CardContent>
                  {data?.trend.length ? (
                    <ChartContainer
                      className='h-[220px] w-full'
                      config={{
                        attempts: {
                          label: t('Channel attempts'),
                          color: '#2563eb',
                        },
                        failures: { label: t('Failures'), color: '#dc2626' },
                      }}
                    >
                      <LineChart data={data.trend}>
                        <CartesianGrid strokeDasharray='3 3' />
                        <XAxis
                          dataKey='at'
                          tickFormatter={(at) =>
                            new Date(Number(at) * 1000).toLocaleTimeString(
                              locale,
                              {
                                hour: '2-digit',
                                minute: '2-digit',
                              }
                            )
                          }
                        />
                        <YAxis />
                        <Tooltip
                          labelFormatter={(at) =>
                            new Date(Number(at) * 1000).toLocaleString(locale)
                          }
                          formatter={(v) => formatNumber(Number(v), locale)}
                        />
                        <Line
                          type='monotone'
                          dataKey='attempts'
                          name={t('Channel attempts')}
                          stroke='#2563eb'
                          dot={false}
                        />
                        <Line
                          type='monotone'
                          dataKey='failures'
                          name={t('Failures')}
                          stroke='#dc2626'
                          dot={false}
                        />
                      </LineChart>
                    </ChartContainer>
                  ) : (
                    <p className='text-muted-foreground py-16 text-center'>
                      {t('No data')}
                    </p>
                  )}
                </CardContent>
              </Card>
              <Card>
                <CardHeader>
                  <CardTitle>{t('CPA execution routes')}</CardTitle>
                  <CardDescription>
                    {t('Actual BPS, Ticket and standard executor outcomes')}
                  </CardDescription>
                </CardHeader>
                <CardContent>
                  {data?.cpa.length ? (
                    data.cpa.map((r) => (
                      <div
                        key={r.route + r.outcome}
                        className='flex justify-between border-b py-3 text-sm'
                      >
                        <span>
                          {r.route.toUpperCase()} · {t(r.outcome)}
                        </span>
                        <span>{fmt(r.count)}</span>
                      </div>
                    ))
                  ) : (
                    <p className='text-muted-foreground py-8'>{t('No data')}</p>
                  )}
                </CardContent>
              </Card>
            </div>
            {data?.alerts.length ? (
              <div
                role='alert'
                className='border-destructive/30 rounded-xl border p-4 text-sm'
              >
                {t('High failure rate alert')}:{' '}
                {data.alerts.map((id) => `#${id}`).join(', ')}
              </div>
            ) : null}
            <Card>
              <CardHeader>
                <CardTitle className='flex items-center gap-2'>
                  <FlaskConical className='size-4' />
                  {t('Independent quality tests')}
                </CardTitle>
                <CardDescription>
                  {t(
                    'Candy checks the configured expected answer; SVG validation checks rendering safety. Visual ratings are optional.'
                  )}
                </CardDescription>
              </CardHeader>
              <CardContent className='space-y-5'>
                {admin && (
                  <>
                    <div className='grid gap-3 sm:grid-cols-2 lg:grid-cols-3'>
                      <div>
                        <Label htmlFor='monitor-channel'>{t('Channel')}</Label>
                        <NativeSelect
                          id='monitor-channel'
                          className='w-full'
                          value={channel}
                          onChange={(e) => {
                            setChannel(e.target.value)
                            setModel('')
                          }}
                        >
                          <option value=''>{t('Select a channel')}</option>
                          {rows
                            .filter((r) => r.status === 1)
                            .map((r) => (
                              <option key={r.id} value={r.id}>
                                {r.name}
                              </option>
                            ))}
                        </NativeSelect>
                      </div>
                      <div>
                        <Label htmlFor='monitor-model'>{t('Model')}</Label>
                        <NativeSelect
                          id='monitor-model'
                          className='w-full'
                          value={model}
                          onChange={(e) => setModel(e.target.value)}
                          disabled={!channel}
                        >
                          <option value=''>{t('Select a model')}</option>
                          {rows
                            .find((r) => r.id === Number(channel))
                            ?.models.map((m) => (
                              <option key={m}>{m}</option>
                            ))}
                        </NativeSelect>
                      </div>
                      <div>
                        <Label htmlFor='monitor-kind'>{t('Test')}</Label>
                        <NativeSelect
                          id='monitor-kind'
                          className='w-full'
                          value={kind}
                          onChange={(e) => {
                            setKind(e.target.value)
                            setPromptDraft(null)
                            setExpectedAnswer('21')
                          }}
                        >
                          <option value='candy'>{t('Candy logic test')}</option>
                          <option value='pelican'>
                            {t('Pelican bicycle drawing')}
                          </option>
                        </NativeSelect>
                      </div>
                      <div>
                        <Label htmlFor='monitor-protocol'>
                          {t('Protocol')}
                        </Label>
                        <NativeSelect
                          id='monitor-protocol'
                          className='w-full'
                          value={protocol}
                          onChange={(e) => setProtocol(e.target.value)}
                        >
                          <option value='responses'>Responses</option>
                          <option value='chat'>Chat Completions</option>
                        </NativeSelect>
                      </div>
                      <div>
                        <Label htmlFor='monitor-effort'>
                          {t('Reasoning effort')}
                        </Label>
                        <NativeSelect
                          id='monitor-effort'
                          className='w-full'
                          value={effort}
                          onChange={(e) => setEffort(e.target.value)}
                        >
                          {[
                            'low',
                            'medium',
                            'high',
                            'xhigh',
                            'max',
                            'ultra',
                          ].map((v) => (
                            <option key={v}>{v}</option>
                          ))}
                        </NativeSelect>
                      </div>
                      <div>
                        <Label htmlFor='monitor-interval'>
                          {t('Schedule interval')}
                        </Label>
                        <Input
                          id='monitor-interval'
                          type='number'
                          min={5}
                          max={1440}
                          step={1}
                          value={interval}
                          aria-invalid={!validInterval}
                          onChange={(e) => setInterval(e.target.value)}
                        />
                        <p className='text-muted-foreground mt-1 text-xs'>
                          {t(
                            '5–1440 minutes; group statistics still update every 5 minutes.'
                          )}
                        </p>
                      </div>
                    </div>
                    <div className='space-y-2'>
                      <div className='flex items-center justify-between gap-2'>
                        <Label htmlFor='monitor-prompt'>
                          {t('Test prompt')}
                        </Label>
                        <Button
                          variant='ghost'
                          size='sm'
                          onClick={() => setPromptDraft(null)}
                        >
                          {t('Restore default prompt')}
                        </Button>
                      </div>
                      <Textarea
                        id='monitor-prompt'
                        rows={5}
                        maxLength={4000}
                        value={prompt}
                        onChange={(e) => setPromptDraft(e.target.value)}
                      />
                      {kind === 'candy' && (
                        <div className='space-y-2'>
                          <Label htmlFor='monitor-answer'>
                            {t('Expected answer')}
                          </Label>
                          <Input
                            id='monitor-answer'
                            type='number'
                            step={1}
                            min={-1000000000}
                            max={1000000000}
                            value={expectedAnswer}
                            aria-invalid={!validAnswer}
                            onChange={(e) => setExpectedAnswer(e.target.value)}
                          />
                          <p className='text-muted-foreground text-xs'>
                            {t(
                              'Candy responses must include a JSON object with an integer "answer"; keep the expected answer consistent with the prompt.'
                            )}
                          </p>
                        </div>
                      )}
                    </div>
                    <div className='flex flex-wrap gap-2'>
                      <Button
                        disabled={!formValid || submit.isPending}
                        onClick={() => {
                          setAction('test')
                          setConfirm(true)
                        }}
                      >
                        {t('Run test')}
                      </Button>
                      <Button
                        variant='outline'
                        disabled={
                          !formValid || !validInterval || submit.isPending
                        }
                        onClick={() => {
                          setAction('schedule')
                          setConfirm(true)
                        }}
                      >
                        {editingSchedule
                          ? t('Save scheduled test')
                          : t('Enable scheduled tests')}
                      </Button>
                      {editingSchedule && (
                        <Button
                          variant='ghost'
                          onClick={() => setEditingSchedule(null)}
                        >
                          {t('Cancel editing')}
                        </Button>
                      )}
                    </div>
                    <p className='text-muted-foreground text-xs'>
                      {t(
                        'Tests call the selected upstream directly, may incur provider charges, and do not affect NewAPI business failure rates. Scheduled tests are off by default.'
                      )}
                    </p>
                  </>
                )}
                <StaticDataTable
                  columns={evalColumns}
                  data={query.data?.evaluations ?? []}
                  getRowKey={(r) => r.id}
                  emptyContent={t('No quality tests yet')}
                />
                {admin &&
                  query.data?.schedules.map((s) => (
                    <div
                      key={s.id}
                      className='flex flex-wrap items-center justify-between gap-2 rounded-lg border p-3 text-sm'
                    >
                      <span>
                        {s.model} ·{' '}
                        {s.kind === 'candy'
                          ? t('Candy logic test')
                          : t('Pelican bicycle drawing')}{' '}
                        · {formatNumber(s.interval_minutes, locale)}{' '}
                        {t('minutes')} ·{' '}
                        {s.enabled ? t('Enabled') : t('Disabled')}
                        {s.enabled && (
                          <span className='text-muted-foreground block text-xs'>
                            {t('Next test')}:{' '}
                            {new Date(s.next_at).toLocaleString(locale)}
                          </span>
                        )}
                      </span>
                      <div className='flex gap-2'>
                        <Button
                          variant='outline'
                          size='sm'
                          onClick={() => {
                            setEditingSchedule(s)
                            setChannel(String(s.channel_id))
                            setModel(s.model)
                            setKind(s.kind)
                            setProtocol(s.protocol)
                            setEffort(s.effort)
                            setInterval(String(s.interval_minutes))
                            setPromptDraft(s.prompt ?? null)
                            setExpectedAnswer(String(s.expected_answer ?? 21))
                            document
                              .querySelector<HTMLTextAreaElement>(
                                '#monitor-prompt'
                              )
                              ?.focus()
                          }}
                        >
                          {t('Edit')}
                        </Button>
                        <Button
                          variant='outline'
                          size='sm'
                          disabled={scheduleToggle.isPending}
                          onClick={() => scheduleToggle.mutate(s)}
                        >
                          {s.enabled ? t('Disable') : t('Enable')}
                        </Button>
                      </div>
                    </div>
                  ))}
              </CardContent>
            </Card>
            {admin && selected && (
              <Card>
                <CardHeader>
                  <CardTitle>
                    {t('Test details')} · {selected.model}
                  </CardTitle>
                  <CardDescription>
                    {new Date(selected.created_at).toLocaleString(locale)} ·{' '}
                    {formatNumber(selected.latency_seconds, locale)} s
                  </CardDescription>
                </CardHeader>
                <CardContent className='space-y-4'>
                  <div className='space-y-1'>
                    <p className='text-sm font-medium'>{t('Test prompt')}</p>
                    <p className='text-muted-foreground text-sm break-words whitespace-pre-wrap'>
                      {selected.prompt || t('Prompt not recorded')}
                    </p>
                  </div>
                  {selected.error_kind && (
                    <p role='alert'>{selected.error_kind}</p>
                  )}
                  {selected.svg && selected.renderable && (
                    <iframe
                      title={t('Pelican SVG preview')}
                      sandbox=''
                      referrerPolicy='no-referrer'
                      srcDoc={svgDocument(selected.svg)}
                      className='h-96 w-full rounded-lg border bg-white'
                    />
                  )}
                  {selected.answer && (
                    <details>
                      <summary className='cursor-pointer text-sm'>
                        {t('Raw answer')}
                      </summary>
                      <pre className='bg-muted mt-2 max-h-80 overflow-auto rounded-lg p-4 text-xs break-all whitespace-pre-wrap'>
                        {selected.answer}
                      </pre>
                    </details>
                  )}
                  {selected.kind === 'pelican' && selected.renderable && (
                    <div className='flex flex-wrap items-end gap-3'>
                      <div>
                        <Label htmlFor='monitor-score'>
                          {t('Human visual score')}
                        </Label>
                        <NativeSelect
                          id='monitor-score'
                          value={score}
                          onChange={(e) => setScore(e.target.value)}
                        >
                          {[1, 2, 3, 4, 5].map((v) => (
                            <option key={v}>{v}</option>
                          ))}
                        </NativeSelect>
                      </div>
                      <div className='min-w-0 flex-1'>
                        <Label htmlFor='monitor-review'>{t('Review')}</Label>
                        <Input
                          id='monitor-review'
                          maxLength={2000}
                          value={review}
                          onChange={(e) => setReview(e.target.value)}
                        />
                      </div>
                      <Button
                        disabled={saveReview.isPending}
                        onClick={() => saveReview.mutate()}
                      >
                        {t('Save review')}
                      </Button>
                    </div>
                  )}
                  <Button variant='outline' onClick={() => setSelected(null)}>
                    {t('Close')}
                  </Button>
                </CardContent>
              </Card>
            )}
            <p className='text-muted-foreground text-xs leading-relaxed'>
              {t(
                'Monitoring begins after activation; historical logs are not imported. Counts use Prometheus estimates. Traces sample 20% of business requests. Keys and request bodies are not recorded.'
              )}{' '}
              {data && new Date(data.started_at).toLocaleString(locale)}
            </p>
          </>
        )}
        <ConfirmDialog
          open={confirm}
          onOpenChange={setConfirm}
          title={
            action === 'test'
              ? t('Run quality test?')
              : t('Save scheduled test?')
          }
          desc={t(
            'This sends real requests to the selected provider and may incur charges.'
          )}
          confirmText={t('Confirm')}
          handleConfirm={() => submit.mutate()}
          isLoading={submit.isPending}
        />
      </div>
    </Main>
  )
}
