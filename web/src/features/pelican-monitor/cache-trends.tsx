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
import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { CartesianGrid, Line, LineChart, XAxis, YAxis } from 'recharts'

import { EmptyState } from '@/components/empty-state'
import { Button } from '@/components/ui/button'
import { ButtonGroup } from '@/components/ui/button-group'
import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
} from '@/components/ui/chart'
import { toIntlLocale } from '@/i18n/languages'

import {
  aggregateGroupRates,
  intervalMinutes,
  type RateSample,
  type Hours,
} from './group-rate-series'

export function CacheTrendCharts({
  group,
  points,
  finishedAt,
}: {
  group: string
  points: RateSample[]
  finishedAt: number
}) {
  const { t, i18n } = useTranslation()
  const [hours, setHours] = useState<Hours>(24)
  const data = useMemo(
    () => aggregateGroupRates(points, finishedAt, hours),
    [points, finishedAt, hours]
  )
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const percent = new Intl.NumberFormat(locale, {
    style: 'percent',
    maximumSignificantDigits: 3,
  })
  const time = new Intl.DateTimeFormat(locale, {
    hour: '2-digit',
    minute: '2-digit',
  })
  const date = new Intl.DateTimeFormat(locale, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })
  const title = t('Cache hit rate trend')
  const cacheLabel = t('Cache hit rate')
  const ranges = [
    [1, t('Last hour')],
    [6, t('Last 6 hours')],
    [24, t('Last 24 hours')],
  ] as const

  return (
    <div className='min-w-0 space-y-5'>
      <div className='flex flex-wrap items-center justify-between gap-4'>
        <dl>
          <dt className='text-muted-foreground text-sm'>{cacheLabel}</dt>
          <dd className='mt-1 text-3xl font-semibold tracking-tight tabular-nums'>
            {data.cacheHitRate == null
              ? t('Unknown')
              : percent.format(data.cacheHitRate / 100)}
          </dd>
        </dl>
        <ButtonGroup aria-label={t('Time range')}>
          {ranges.map(([value, label]) => (
            <Button
              key={value}
              size='sm'
              variant={value === hours ? 'default' : 'outline'}
              aria-pressed={value === hours}
              onClick={() => setHours(value)}
            >
              {label}
            </Button>
          ))}
        </ButtonGroup>
      </div>
      <figure aria-label={`${group} · ${title}`} className='min-w-0 space-y-3'>
        <figcaption className='flex flex-wrap items-center justify-between gap-x-4 gap-y-2'>
          <span className='text-sm font-medium'>{title}</span>
          <span className='text-muted-foreground text-xs'>
            {t('{{minutes}}-minute intervals', {
              minutes: intervalMinutes[hours],
            })}
          </span>
        </figcaption>
        {data.points.some((point) => point.cache_hit_rate != null) ? (
          <ChartContainer
            config={{
              cache_hit_rate: { label: cacheLabel, color: 'var(--success)' },
            }}
            className='aspect-auto h-56 w-full sm:h-64'
          >
            <LineChart
              data={data.points}
              accessibilityLayer
              margin={{ left: 0, right: 10, top: 8, bottom: 0 }}
            >
              <CartesianGrid vertical={false} />
              <XAxis
                dataKey='timestamp'
                tickFormatter={(value: number) => time.format(value * 1000)}
                minTickGap={28}
                tickLine={false}
                axisLine={false}
                tickMargin={8}
              />
              <YAxis
                domain={[0, 100]}
                ticks={[0, 25, 50, 75, 100]}
                width={42}
                tickFormatter={(value: number) => percent.format(value / 100)}
                tickLine={false}
                axisLine={false}
              />
              <ChartTooltip
                content={
                  <ChartTooltipContent
                    labelFormatter={(_, payload) =>
                      date.format(payload[0].payload.timestamp * 1000)
                    }
                    formatter={(value, name) => (
                      <span className='flex w-full items-center justify-between gap-3'>
                        {name}
                        <span className='font-medium tabular-nums'>
                          {percent.format(Number(value) / 100)}
                        </span>
                      </span>
                    )}
                  />
                }
              />
              <Line
                type='linear'
                dataKey='cache_hit_rate'
                name={cacheLabel}
                stroke='var(--color-cache_hit_rate)'
                strokeWidth={2}
                dot={{ r: 2.5, fill: 'var(--color-cache_hit_rate)' }}
                connectNulls={false}
                isAnimationActive={false}
              />
            </LineChart>
          </ChartContainer>
        ) : (
          <EmptyState
            className='min-h-28'
            title={t('No cache trend data for this period')}
          />
        )}
      </figure>
    </div>
  )
}
