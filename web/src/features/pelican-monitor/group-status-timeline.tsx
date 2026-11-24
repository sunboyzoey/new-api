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

import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { toIntlLocale } from '@/i18n/languages'
import { cn } from '@/lib/utils'

import type { GroupSummary } from './api'
import { aggregateGroupRates, intervalMinutes } from './group-rate-series'

export function GroupStatusTimeline(props: {
  group: GroupSummary
  errorLabel: string
}) {
  const { t, i18n } = useTranslation()
  const { trend, finished_at: finishedAt } = props.group
  const [selectedSlot, setSelectedSlot] = useState<number>()
  const [hoverSlot, setHoverSlot] = useState<number>()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const minutes = intervalMinutes[24]
  const { intervals, states, startLabel, endLabel } = useMemo(() => {
    const { points } = aggregateGroupRates(trend, finishedAt, 24)
    const dateTime = new Intl.DateTimeFormat(locale, {
      month: 'short',
      day: 'numeric',
      hour: '2-digit',
      minute: '2-digit',
    })
    const percent = new Intl.NumberFormat(locale, {
      style: 'percent',
      maximumSignificantDigits: 3,
    })
    const states = {
      success: { label: t('No failures'), color: 'bg-success', height: 'h-5' },
      partial: {
        label: t('Some failures'),
        color: 'bg-warning',
        height: 'h-4',
      },
      failed: {
        label: t('All failed'),
        color: 'bg-destructive',
        height: 'h-3',
      },
      unknown: {
        label: t('No data'),
        color: 'bg-muted-foreground/40',
        height: 'h-2',
      },
    }
    const intervals = points.map((point) => {
      const rate = point.error_rate
      let status = states.unknown
      if (rate === 0) status = states.success
      else if (rate === 100) status = states.failed
      else if (rate != null) status = states.partial
      const start = point.timestamp * 1000
      const range = dateTime.formatRange(start, start + minutes * 60_000)
      let detail = `${range} · ${status.label}`
      if (rate != null) {
        detail += ` · ${props.errorLabel} ${percent.format(rate / 100)}`
      }
      return { minutesAgo: (finishedAt - point.timestamp) / 60, status, detail }
    })
    return {
      intervals,
      states,
      startLabel: dateTime.format(points[0].timestamp * 1000),
      endLabel: dateTime.format(finishedAt * 1000),
    }
  }, [trend, finishedAt, locale, t, props.errorLabel, minutes])
  // The rolling window shifts at each refresh. Keep focus on the same slot.
  const selected = selectedSlot ?? intervals.length - 1
  const preview = intervals[hoverSlot ?? selected]
  const title = t('Group status history')

  return (
    <figure
      aria-label={`${props.group.group} · ${title}`}
      className='min-w-0 space-y-2'
    >
      <figcaption className='flex flex-wrap items-baseline justify-between gap-2'>
        <span className='text-sm font-medium'>{title}</span>
        <span className='text-muted-foreground text-xs'>
          {t('{{minutes}}-minute intervals', { minutes })}
        </span>
      </figcaption>
      <ToggleGroup
        spacing={1}
        aria-label={title}
        value={[String(selected)]}
        onValueChange={(values) => {
          if (values.length) setSelectedSlot(Number(values[0]))
        }}
        onPointerLeave={() => setHoverSlot(undefined)}
        onKeyDown={() => setHoverSlot(undefined)}
        className='w-full gap-px sm:gap-0.5'
      >
        {intervals.map(({ minutesAgo, status, detail }, slot) => (
          <ToggleGroupItem
            key={minutesAgo}
            value={String(slot)}
            aria-label={detail}
            onFocus={() => {
              setSelectedSlot(slot)
              setHoverSlot(undefined)
            }}
            onPointerEnter={(event) => {
              if (event.pointerType === 'mouse') setHoverSlot(slot)
            }}
            className='aria-pressed:ring-ring h-8 min-w-0 flex-1 items-end rounded-xs px-0 pb-1 aria-pressed:ring-1'
          >
            <span
              aria-hidden='true'
              className={cn('w-full rounded-xs', status.color, status.height)}
            />
          </ToggleGroupItem>
        ))}
      </ToggleGroup>
      <div className='text-muted-foreground flex justify-between gap-2 text-xs tabular-nums'>
        <span>{startLabel}</span>
        <span>{endLabel}</span>
      </div>
      <p className='text-xs tabular-nums'>{preview.detail}</p>
      <div className='text-muted-foreground flex flex-wrap gap-x-3 gap-y-1 text-xs'>
        {Object.values(states).map((status) => (
          <span key={status.color} className='inline-flex items-center gap-1'>
            <span
              aria-hidden='true'
              className={cn('h-2 w-1 rounded-xs', status.color)}
            />
            {status.label}
          </span>
        ))}
      </div>
    </figure>
  )
}
