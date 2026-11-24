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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Play, Settings2 } from 'lucide-react'
import { lazy, Suspense, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { SectionPageLayout } from '@/components/layout/components/section-page-layout'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Combobox } from '@/components/ui/combobox'

import {
  getCacheHealth,
  getPelicanGroups,
  startGroupTest,
  type GroupSummary,
} from './api'
import { GroupTestDisclosure } from './group-history'

const CacheTrendCharts = lazy(() =>
  import('./cache-trends').then((module) => ({
    default: module.CacheTrendCharts,
  }))
)
const MonitorConfiguration = lazy(() =>
  import('./config').then((module) => ({
    default: module.MonitorConfiguration,
  }))
)

export function GroupCacheHealthPage() {
  const { t } = useTranslation()
  const [selected, setSelected] = useState<string | null>(null)
  const [configPanel, setConfigPanel] = useState<
    'unopened' | 'open' | 'closed'
  >('unopened')
  const client = useQueryClient()
  const [requestedHistory, setRequestedHistory] = useState<{
    target: string
    revision: number
  }>()
  const manualTest = useMutation({
    mutationFn: (group: Pick<GroupSummary, 'id' | 'group'>) =>
      startGroupTest(group.id),
    onSuccess: (result, group) => {
      if (result.created) {
        toast.success(t('Group test queued'), { description: group.group })
      } else {
        toast.info(t('A test for this group is already running'), {
          description: group.group,
        })
      }
      // Reopen the first page even when the administrator was browsing older results.
      void client.invalidateQueries({
        queryKey: ['pelican-results', group.id],
        refetchType: 'none',
      })
      setRequestedHistory((previous) => ({
        target: group.id,
        revision: (previous?.revision ?? 0) + 1,
      }))
      void client.invalidateQueries({ queryKey: ['pelican-groups'] })
    },
  })
  const local = useQuery({
    queryKey: ['group-cache-health'],
    queryFn: ({ signal }) => getCacheHealth(signal),
    refetchInterval: 60_000,
  })
  const targets = useQuery({
    queryKey: ['pelican-groups'],
    queryFn: ({ signal }) => getPelicanGroups(signal),
    refetchInterval: 30_000,
  })
  const localTargets = new Set<string>()
  const options: {
    value: string
    name: string
    label: string
    target?: GroupSummary
  }[] = []
  for (const target of targets.data?.groups ?? []) {
    const isProbe = target.cache_source === 'probe'
    if (!isProbe) localTargets.add(target.group)
    const source = isProbe ? t('Test requests') : t('Local group traffic')
    options.push({
      value: `target:${target.id}`,
      name: target.group,
      label: `${target.group} · ${source}`,
      target,
    })
  }
  for (const row of local.data?.groups ?? []) {
    if (localTargets.has(row.group)) continue
    const name = row.group || t('Unknown')
    options.push({
      value: `traffic:${row.group}`,
      name: row.group,
      label: `${name} · ${t('Local group traffic')}`,
    })
  }
  const current =
    options.find((option) => option.value === selected) ?? options[0]
  const target = current?.target
  const historyRevision =
    requestedHistory && requestedHistory.target === target?.id
      ? requestedHistory.revision
      : 0
  const useTraffic = target?.cache_source !== 'probe' && local.data != null
  const series = useTraffic
    ? local.data.trends?.find((row) => row.group === current?.name)?.points
    : target?.trend
  const finishedAt = useTraffic ? local.data.finished_at : target?.finished_at
  const truncated = useTraffic ? local.data.truncated : target?.truncated
  return (
    <SectionPageLayout>
      <SectionPageLayout.Title>
        {t('Group cache health')}
      </SectionPageLayout.Title>
      <SectionPageLayout.Actions>
        <Button
          size='sm'
          variant='outline'
          aria-expanded={configPanel === 'open'}
          aria-controls='group-test-config'
          onClick={() =>
            setConfigPanel(configPanel === 'open' ? 'closed' : 'open')
          }
        >
          <Settings2 aria-hidden='true' />
          {t('Configure group tests')}
        </Button>
      </SectionPageLayout.Actions>
      <SectionPageLayout.Content>
        <div className='space-y-4'>
          <div id='group-test-config' hidden={configPanel !== 'open'}>
            {configPanel !== 'unopened' && (
              <Suspense fallback={<LoadingState />}>
                <MonitorConfiguration />
              </Suspense>
            )}
          </div>
          {local.isError && !local.data && (
            <ErrorState
              className='min-h-24'
              description={t('Cache statistics unavailable')}
              onRetry={() => void local.refetch()}
            />
          )}
          {targets.isError && !targets.data && (
            <ErrorState
              className='min-h-24'
              description={t('Test results could not be loaded')}
              onRetry={() => void targets.refetch()}
            />
          )}
          {current && (
            <Card className='gap-0 pb-0'>
              <CardContent className='space-y-5 px-4 pb-5 sm:px-6'>
                <div className='flex flex-wrap items-center gap-3'>
                  {options.length > 1 ? (
                    <Combobox
                      aria-label={t('Choose Group')}
                      options={options}
                      value={current.value}
                      onValueChange={setSelected}
                      searchPlaceholder={t('Search groups...')}
                      emptyText={t('No group found.')}
                      className='w-full max-w-md'
                    />
                  ) : (
                    <div>
                      <h3 className='text-lg font-semibold wrap-anywhere'>
                        {current.name || t('Unknown')}
                      </h3>
                      <span className='text-muted-foreground text-xs'>
                        {target?.cache_source === 'probe'
                          ? t('Test requests')
                          : t('Local group traffic')}
                      </span>
                    </div>
                  )}
                  {target && (
                    <div className='flex flex-wrap items-center gap-2'>
                      <Button
                        size='sm'
                        variant='outline'
                        disabled={
                          manualTest.isPending || target.models.length === 0
                        }
                        onClick={() =>
                          manualTest.mutate({
                            id: target.id,
                            group: target.group,
                          })
                        }
                      >
                        <Play aria-hidden='true' />
                        {manualTest.isPending &&
                        manualTest.variables.id === target.id
                          ? t('Testing...')
                          : t('Test now')}
                      </Button>
                      <span className='text-muted-foreground text-xs'>
                        {t('Current models: {{count}}', {
                          count: target.models.length,
                        })}
                      </span>
                    </div>
                  )}
                </div>
                {target?.cache_source === 'unavailable' && !useTraffic ? (
                  <p className='text-muted-foreground text-sm'>
                    {t('Cache statistics unavailable')}
                  </p>
                ) : (
                  <Suspense fallback={<LoadingState />}>
                    <CacheTrendCharts
                      group={current.name || t('Unknown')}
                      points={series ?? []}
                      finishedAt={finishedAt ?? 0}
                    />
                  </Suspense>
                )}
                {truncated && (
                  <p role='status' className='text-warning text-xs'>
                    {target?.cache_source === 'probe'
                      ? t('Statistics truncated')
                      : t(
                          'High traffic: statistics cover only the latest 50,000 log entries.'
                        )}
                  </p>
                )}
              </CardContent>
              {target && (
                <GroupTestDisclosure
                  key={`${target.id}:${historyRevision}`}
                  group={target}
                  defaultExpanded={historyRevision > 0}
                />
              )}
            </Card>
          )}
          {!current && (local.isPending || targets.isPending) && (
            <LoadingState />
          )}
          {!current && local.isSuccess && targets.isSuccess && (
            <EmptyState title={t('No Data')} />
          )}
        </div>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
