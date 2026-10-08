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
import { useQuery } from '@tanstack/react-query'
import {
  Activity,
  RefreshCw,
  CheckCircle2,
  CircleHelp,
  FlaskConical,
} from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { ErrorState } from '@/components/error-state'
import { PublicLayout } from '@/components/layout'
import { LoadingState } from '@/components/loading-state'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Card,
  CardHeader,
  CardTitle,
  CardContent,
  CardDescription,
} from '@/components/ui/card'
import { metricText } from '@/features/channel-monitor/presentation'
import { toIntlLocale } from '@/i18n/languages'
import { api } from '@/lib/api'
import { formatNumber } from '@/lib/format'

import { DrawingsGallery } from './drawings-gallery'
import { resultLabel, type QualityPoint } from './presentation'
import { RequestOutcomes } from './request-outcomes'
import { ResultHistory } from './result-history'

type QualityResult = {
  model: string
  kind: string
  passed: number
  failed: number
  errors: number
  ungraded: number
  history: QualityPoint[]
}
type GroupHealth = {
  name: string
  models: string[]
  requests: number
  failures: number
  failure_rate: number | null
  p95_seconds: number | null
  ttft_p95_seconds: number | null
  average_seconds?: number | null
  request_history?: import('./request-outcomes').RequestPoint[]
  quality?: QualityResult[]
}
type Response = {
  success: boolean
  message: string
  data: {
    interval_seconds?: number
    updated_at?: string
    next_update_at?: string
    available: boolean
    quality_available?: boolean
    groups: GroupHealth[]
  }
}

function QualitySummary(props: { result: QualityResult; available: boolean }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const r = props.result
  const last = r.history.at(-1)
  const graded = r.passed + r.failed
  const title =
    r.kind === 'candy' ? t('Candy logic test') : t('Pelican bicycle drawing')
  let status = t('No tests yet')
  if (!props.available) status = t('Test results unavailable')
  else if (last) {
    status = `${t('Latest result')}: ${resultLabel(last.state, r.kind, t)}`
  }
  const history = props.available ? r.history : []
  return (
    <div
      className='space-y-3 rounded-xl border p-4'
      data-testid='quality-summary'
    >
      <div className='flex flex-wrap items-center justify-between gap-2'>
        <span className='text-sm font-medium'>{title}</span>
        <Badge variant='outline' className='text-xs'>
          {status}
        </Badge>
      </div>
      <div className='flex flex-wrap items-center justify-between gap-2 text-xs'>
        <span className='text-muted-foreground'>
          {r.kind === 'candy' ? t('Accuracy') : t('Safe rendering')}{' '}
          <strong className='text-foreground ml-1 font-semibold'>
            {props.available && graded > 0
              ? `${formatNumber(r.passed, locale)}/${formatNumber(graded, locale)}`
              : '—'}
          </strong>
          {props.available && graded > 0 && r.kind === 'candy' && (
            <span className='text-muted-foreground ml-1'>
              ({formatNumber((100 * r.passed) / graded, locale)}%)
            </span>
          )}
        </span>
        {props.available &&
          r.kind === 'pelican' &&
          last?.score !== undefined && (
            <span className='text-muted-foreground'>
              {t('Human drawing score')}{' '}
              <strong className='text-foreground'>{last.score}/5</strong>
            </span>
          )}
      </div>
      {props.available && r.kind === 'pelican' && history.length > 0 && (
        <div className='text-muted-foreground text-xs'>
          {t('Generation completed')}:{' '}
          <strong className='text-foreground'>
            {graded + r.ungraded}/{history.length}
          </strong>
        </div>
      )}
      <ResultHistory history={history} kind={r.kind} />
      <div className='text-muted-foreground flex flex-wrap justify-between gap-2 text-xs'>
        <span>
          {t('Recent tests')}: {formatNumber(history.length, locale)}
        </span>
        {props.available && r.errors > 0 && (
          <span className='text-amber-600'>
            {t('Test error')}: {formatNumber(r.errors, locale)}
          </span>
        )}
        {props.available && r.ungraded > 0 && (
          <span>
            {t('Not graded')}: {formatNumber(r.ungraded, locale)}
          </span>
        )}
      </div>
    </div>
  )
}
function GroupCard(props: {
  row: GroupHealth
  available: boolean
  qualityAvailable: boolean
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const row = props.row
  const success =
    props.available && row.failure_rate !== null
      ? Math.max(0, Math.min(100, 100 - row.failure_rate))
      : null
  const StatusIcon = success === null ? CircleHelp : CheckCircle2
  const empty = (model: string, kind: string): QualityResult => ({
    model,
    kind,
    passed: 0,
    failed: 0,
    errors: 0,
    ungraded: 0,
    history: [],
  })
  return (
    <Card className='overflow-hidden'>
      <CardHeader className='gap-3'>
        <div className='flex flex-wrap items-center justify-between gap-2'>
          <CardTitle className='text-xl'>{row.name}</CardTitle>
          <Badge variant='outline' className='gap-1.5'>
            <StatusIcon className='size-3.5' />
            {success === null
              ? t('No data')
              : `${t('Business success rate')} ${formatNumber(success, locale)}%`}
          </Badge>
        </div>
        <CardDescription className='break-words'>
          {row.models.join(' · ')}
        </CardDescription>
      </CardHeader>
      <CardContent className='space-y-6'>
        <div className='space-y-6'>
          <p className='text-muted-foreground text-xs leading-relaxed'>
            {t(
              'Business statistics cover requests through NewAPI for this group in the last hour. Model tests are counted separately.'
            )}
          </p>
          {props.available && row.requests === 0 && (
            <p className='text-muted-foreground text-sm'>
              {t(
                'No business request samples for this group in the last hour; success and failure rates are unavailable.'
              )}
            </p>
          )}
          <RequestOutcomes
            history={row.request_history}
            available={props.available}
          />
          <div className='grid grid-cols-2 gap-4 sm:grid-cols-3 xl:grid-cols-7'>
            {[
              [
                t('Requests'),
                props.available ? formatNumber(row.requests, locale) : '—',
              ],
              [
                t('Success rate'),
                success === null
                  ? t('No data')
                  : `${metricText(success, true, locale)}%`,
              ],
              [
                t('Failed requests'),
                props.available ? formatNumber(row.failures, locale) : '—',
              ],
              [
                t('Failure rate'),
                props.available && row.failure_rate !== null
                  ? `${metricText(row.failure_rate, true, locale)}%`
                  : t('No data'),
              ],
              [
                t('Average duration'),
                props.available && row.average_seconds != null
                  ? `${metricText(row.average_seconds, true, locale)} s`
                  : '—',
              ],
              [
                'P95',
                props.available && row.p95_seconds !== null
                  ? `${metricText(row.p95_seconds, true, locale)} s`
                  : '—',
              ],
              [
                'TTFT P95',
                props.available && row.ttft_p95_seconds !== null
                  ? `${metricText(row.ttft_p95_seconds, true, locale)} s`
                  : '—',
              ],
            ].map(([label, value]) => (
              <div key={label}>
                <div className='text-muted-foreground text-xs'>{label}</div>
                <div className='mt-1 text-lg font-semibold tabular-nums'>
                  {value}
                </div>
              </div>
            ))}
          </div>
        </div>
        <div className='border-t pt-5'>
          <div className='mb-4 flex items-center gap-2 text-sm font-medium'>
            <FlaskConical className='size-4' />
            {t('Model test results')}
          </div>
          <div className='grid gap-4 lg:grid-cols-3'>
            {row.models.map((model) => (
              <div key={model} className='min-w-0 space-y-3'>
                <h3
                  className='text-muted-foreground truncate text-sm font-medium'
                  title={model}
                >
                  {model}
                </h3>
                {['candy', 'pelican'].map((kind) => (
                  <QualitySummary
                    key={kind}
                    result={
                      row.quality?.find(
                        (r) => r.model === model && r.kind === kind
                      ) ?? empty(model, kind)
                    }
                    available={props.qualityAvailable}
                  />
                ))}
              </div>
            ))}
          </div>
        </div>
        {row.models
          .filter((model) =>
            row.quality?.some(
              (r) => r.model === model && r.kind === 'pelican' && r.passed > 0
            )
          )
          .map((model) => (
            <DrawingsGallery key={model} group={row.name} model={model} />
          ))}
      </CardContent>
    </Card>
  )
}
export function GroupMonitor() {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const query = useQuery({
    queryKey: ['group-monitor'],
    queryFn: async () => {
      const { data } = await api.get<Response>('/api/group-monitor')
      if (!data.success) throw new Error(data.message)
      return data
    },
    refetchInterval: (query) => {
      const data = query.state.data?.data
      if (!data?.updated_at || data.updated_at.startsWith('0001')) return 5000
      if (!data.next_update_at) return 300000
      return Math.max(
        5000,
        Date.parse(data.next_update_at) + 15000 - Date.now()
      )
    },
  })
  const available = query.data?.data.available ?? false
  const rows = query.data?.data.groups ?? []
  const total = rows.reduce((n, r) => n + r.requests, 0)
  const failures = rows.reduce((n, r) => n + r.failures, 0)
  return (
    <PublicLayout>
      <div className='mx-auto flex max-w-7xl flex-col gap-6 pb-12'>
        <div className='flex flex-wrap items-center justify-between gap-4'>
          <div>
            <Activity className='text-primary mb-3 size-6' />
            <h1 className='text-3xl font-semibold tracking-tight'>
              {t('Group monitoring')}
            </h1>
            <p className='text-muted-foreground mt-2 text-sm'>
              {t('Request group monitoring')}
            </p>
          </div>
          <div className='flex gap-2'>
            <div className='text-muted-foreground self-center text-xs'>
              {t('Updated every 5 minutes · fixed last-hour statistics')}
              {query.data?.data.updated_at &&
                !query.data.data.updated_at.startsWith('0001') && (
                  <div className='mt-1'>
                    {t('Last statistics update')}:{' '}
                    {new Date(query.data.data.updated_at).toLocaleString(
                      locale
                    )}
                  </div>
                )}
            </div>
            <Button
              variant='outline'
              size='icon'
              aria-label={t('Refresh')}
              onClick={() => void query.refetch()}
            >
              <RefreshCw className='size-4' />
            </Button>
          </div>
        </div>
        <div className='grid gap-4 sm:grid-cols-3'>
          {[
            [t('Groups'), rows.length],
            [t('Requests'), available ? formatNumber(total, locale) : '—'],
            [
              t('Failure rate'),
              available && total > 0
                ? `${metricText((100 * failures) / total, available, locale)}%`
                : t('No data'),
            ],
          ].map(([label, value]) => (
            <Card key={label}>
              <CardHeader>
                <CardTitle className='text-muted-foreground text-sm'>
                  {label}
                </CardTitle>
              </CardHeader>
              <CardContent className='text-2xl font-semibold'>
                {value}
              </CardContent>
            </Card>
          ))}
        </div>
        {query.isLoading && <LoadingState />}
        {query.isError && <ErrorState />}
        {!query.isLoading && !query.isError && (
          <>
            {!available && (
              <p role='alert' className='text-destructive'>
                {t('Health cannot be determined')}
              </p>
            )}
            {rows.map((row) => (
              <GroupCard
                key={row.name}
                row={row}
                available={available}
                qualityAvailable={query.data?.data.quality_available ?? false}
              />
            ))}
          </>
        )}
        <div className='text-muted-foreground space-y-2 text-xs'>
          <div className='flex flex-wrap items-center gap-4'>
            {[
              ['bg-emerald-500', t('Passed')],
              ['bg-rose-500', t('Failed')],
              ['bg-amber-500', t('Test error')],
              ['bg-slate-400', t('Not graded')],
              ['bg-muted', t('No tests yet')],
            ].map(([color, label]) => (
              <span key={label} className='flex items-center gap-1.5'>
                <span className={`${color} size-2.5 rounded-sm`} />
                {label}
              </span>
            ))}
          </div>
          <p>
            {t(
              'Results summarize tests from enabled channels in this group; each bar is one test, with up to 60 recent results.'
            )}
          </p>
          <p>
            {t(
              'Drawing validation is automatic. Human visual ratings are optional; test errors are separate from invalid drawings.'
            )}
          </p>
          <p>
            {t(
              'Group metrics start with new traffic; previous channel totals are not assigned to groups.'
            )}
          </p>
        </div>
      </div>
    </PublicLayout>
  )
}
