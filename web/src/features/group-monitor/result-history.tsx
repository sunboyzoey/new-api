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
import type { ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import { toIntlLocale } from '@/i18n/languages'
import { cn } from '@/lib/utils'

import { resultLabel, type QualityPoint } from './presentation'

// Pricing's day-based UptimeSparkline cannot represent unknown slots or probe
// errors separately from wrong answers. Reuse Tooltip for this result strip.
export function ResultHistory(props: {
  history: (QualityPoint | null)[]
  kind: string
  label?: string
  emptyLabel?: string
  slotCount?: number
  renderPoint?: (point: QualityPoint) => ReactNode
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const count = props.slotCount ?? 60
  const points = props.history.slice(-count)
  // These fixed slots stay in chronological positions as records arrive.
  const slots = Array.from({ length: count }, (_, position) => ({
    id: `slot-${position}`,
    point: points[position - (count - points.length)] ?? null,
  }))
  return (
    <div
      className='flex h-7 w-full gap-0.5'
      role='img'
      aria-label={props.label ?? t('Test result history')}
    >
      {slots.map(({ point, id }) => (
        <Tooltip key={id}>
          <TooltipTrigger
            render={
              <div
                className={cn(
                  'min-w-0 flex-1 rounded-sm transition-opacity hover:opacity-70',
                  point === null ? 'bg-muted' : undefined,
                  point?.state === 'passed' ? 'bg-emerald-500' : undefined,
                  point?.state === 'failed' ? 'bg-rose-500' : undefined,
                  point?.state === 'error' ? 'bg-amber-500' : undefined,
                  point?.state === 'ungraded' ? 'bg-slate-400' : undefined
                )}
                data-result={point?.state ?? 'empty'}
              />
            }
          />
          <TooltipContent>
            {point === null ? (
              (props.emptyLabel ?? t('No tests yet'))
            ) : (
              <div>
                {props.renderPoint ? (
                  props.renderPoint(point)
                ) : (
                  <>
                    <div>{new Date(point.at).toLocaleString(locale)}</div>
                    <div>{resultLabel(point.state, props.kind, t)}</div>
                    {point.score !== undefined && (
                      <div>
                        {t('Human drawing score')}: {point.score}/5
                      </div>
                    )}
                  </>
                )}
              </div>
            )}
          </TooltipContent>
        </Tooltip>
      ))}
    </div>
  )
}
