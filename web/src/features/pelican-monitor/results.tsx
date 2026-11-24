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
import { Activity, Clock3, Layers } from 'lucide-react'
import type { ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { StatusBadge } from '@/components/status-badge'
import { Card } from '@/components/ui/card'
import { toIntlLocale } from '@/i18n/languages'

import { getPelicanGroups, getPelicanResults, type GroupSummary } from './api'
import {
  GroupTestDisclosure,
  PelicanResultCard,
  PelicanResultGrid,
} from './group-history'
import { GroupStatusTimeline } from './group-status-timeline'
import { HtmlTrial } from './html-trial'
import { reviewedHtmlTrial } from './html-trial-document'
import { useHtmlTrial } from './use-html-trial'

const homepageResultLimit = 20

export function PelicanResults() {
  const { t } = useTranslation()
  const query = useQuery({
    queryKey: ['pelican-groups'],
    queryFn: ({ signal }) => getPelicanGroups(signal),
    refetchInterval: 30_000,
  })
  const history = useQuery({
    queryKey: ['pelican-results', 'homepage'],
    queryFn: ({ signal }) => getPelicanResults(0, signal),
    enabled: !!query.data?.groups.length,
    refetchInterval: 30_000,
  })
  const trialGroup = query.data?.groups.find(
    (group) => group.id === reviewedHtmlTrial.target_id
  )
  const oldestVisible = history.data?.items[homepageResultLimit - 1]
  const trialInRange =
    !oldestVisible || oldestVisible.started_at < reviewedHtmlTrial.finished_at
  const trial = useHtmlTrial(
    history.data && trialInRange ? trialGroup : undefined
  )
  if (query.isPending) return <LoadingState />
  if (query.isError) {
    return (
      <ErrorState
        description={t('Test results could not be loaded')}
        onRetry={() => void query.refetch()}
      />
    )
  }
  const { groups, enabled, interval_minutes: interval } = query.data
  // The public endpoint already orders all groups by start time and ID.
  // The separately reviewed trial has only a completion timestamp.
  const recent = (history.data?.items ?? []).map((row) => ({
    target: row.target_id,
    group: row.group,
    timestamp: row.started_at,
    card: <PelicanResultCard key={row.id} result={row} />,
  }))
  if (trial && trialGroup) {
    const index = recent.findIndex(
      (entry) => entry.timestamp < trial.finished_at
    )
    recent.splice(index < 0 ? recent.length : index, 0, {
      target: trialGroup.id,
      group: trialGroup.group,
      timestamp: trial.finished_at,
      card: <HtmlTrial key='trial' group={trialGroup} />,
    })
  }
  const cardsByGroup = new Map<string, ReactNode[]>()
  const groupNames = new Map(groups.map((group) => [group.id, group.group]))
  for (const { target, group, card } of recent.slice(0, homepageResultLimit)) {
    if (groupNames.get(target) !== group) continue
    const cards = cardsByGroup.get(target) ?? []
    cards.push(card)
    cardsByGroup.set(target, cards)
  }
  let fallback = (
    <EmptyState className='min-h-0 py-4' title={t('No recent test results')} />
  )
  if (history.isPending) {
    fallback = <LoadingState size='sm' className='min-h-0 py-4' />
  } else if (history.isError) {
    fallback = (
      <ErrorState
        className='min-h-0 py-4'
        onRetry={() => void history.refetch()}
      />
    )
  }
  let schedule = t('Scheduled every {{minutes}} minutes', { minutes: interval })
  if (!enabled) schedule = t('Scheduled tests are paused')
  else if (!groups.some((group) => group.enabled)) {
    schedule = t('All monitoring groups are paused')
  }
  return (
    <div className='space-y-6'>
      <header className='flex items-center gap-3 border-b pb-5'>
        <span className='bg-primary/10 text-primary flex size-11 shrink-0 items-center justify-center rounded-xl'>
          <Activity aria-hidden='true' className='size-6' />
        </span>
        <div>
          <h1 className='text-2xl font-semibold tracking-tight sm:text-3xl'>
            {t('Group monitoring')}
          </h1>
          {groups.length > 0 && (
            <p className='text-muted-foreground mt-1 flex items-center gap-1.5 text-sm'>
              <Clock3 aria-hidden='true' className='size-3.5' />
              {schedule}
            </p>
          )}
        </div>
      </header>
      {groups.length === 0 ? (
        <EmptyState
          icon={Layers}
          title={t('No monitoring groups configured')}
          description={t(
            'Group test results will appear here after an administrator adds a monitoring group.'
          )}
          bordered
        />
      ) : (
        <div className='grid grid-cols-[repeat(auto-fit,minmax(min(100%,30rem),1fr))] items-start gap-5'>
          {groups.map((group) => (
            <GroupOverview key={group.id} group={group}>
              {cardsByGroup.has(group.id) ? (
                <PelicanResultGrid>
                  {cardsByGroup.get(group.id)}
                </PelicanResultGrid>
              ) : (
                fallback
              )}
            </GroupOverview>
          ))}
        </div>
      )}
    </div>
  )
}

function GroupOverview({
  group,
  children,
}: {
  group: GroupSummary
  children: ReactNode
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const percent = new Intl.NumberFormat(locale, {
    style: 'percent',
    maximumSignificantDigits: 3,
  })
  const isProbe = group.cache_source === 'probe'
  const available = group.cache_source !== 'unavailable'
  const errorLabel = isProbe ? t('Test failure rate') : t('Request error rate')
  const metrics = [
    { label: t('Cache hit rate'), value: group.cache.cache_hit_rate },
    {
      label: errorLabel,
      value: group.error_rate,
      title: isProbe
        ? undefined
        : t('Each logged request attempt counts, including retries.'),
    },
  ]
  return (
    <section aria-labelledby={`monitor-group-${group.id}`}>
      <Card className='gap-0 py-0'>
        <div className='flex flex-wrap items-start justify-between gap-3 px-4 pt-5 sm:px-6'>
          <div className='min-w-0'>
            <h2
              id={`monitor-group-${group.id}`}
              className='text-xl font-semibold tracking-tight wrap-anywhere'
            >
              {group.group}
            </h2>
            <p className='text-muted-foreground mt-1 text-xs'>
              {isProbe
                ? t('Test requests · last 24 hours')
                : t('Group traffic · last 24 hours')}
            </p>
          </div>
          {!group.enabled && (
            <StatusBadge
              label={t('Scheduled tests are paused')}
              variant='neutral'
              copyable={false}
            />
          )}
        </div>
        <div className='space-y-4 px-4 py-5 sm:px-6'>
          <dl className='bg-muted/40 grid grid-cols-2 divide-x rounded-lg border py-3'>
            {metrics.map(({ label, value, title }) => (
              <div key={label} className='min-w-0 px-3 sm:px-4'>
                <dt className='text-muted-foreground text-xs' title={title}>
                  {label}
                </dt>
                <dd className='mt-1 text-2xl leading-tight font-semibold tracking-tight wrap-anywhere tabular-nums'>
                  {available && value != null
                    ? percent.format(value / 100)
                    : t('Unknown')}
                </dd>
              </div>
            ))}
          </dl>
          {available && group.trend && group.finished_at > 0 && (
            <GroupStatusTimeline group={group} errorLabel={errorLabel} />
          )}
          {!available && (
            <p className='text-muted-foreground text-sm'>
              {t('Cache statistics unavailable')}
            </p>
          )}
          {group.truncated && (
            <p className='text-warning text-xs'>{t('Statistics truncated')}</p>
          )}
        </div>
        <GroupTestDisclosure group={group} defaultExpanded>
          {children}
        </GroupTestDisclosure>
      </Card>
    </section>
  )
}
