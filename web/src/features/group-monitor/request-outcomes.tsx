import { useTranslation } from 'react-i18next'

import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'

import { ResultHistory } from './result-history'

export type RequestPoint = {
  at: number
  requests: number
  failures: number
  failure_rate: number | null
}
export function RequestOutcomes(props: {
  history?: RequestPoint[]
  available: boolean
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const rows = props.history ?? []
  const history = Array.from({ length: 12 }, (_, index) => {
    const r = rows[index]
    if (!props.available || !r || r.requests <= 0) return null
    let state: 'passed' | 'failed' | 'error' = 'passed'
    if (r.failures >= r.requests) state = 'failed'
    else if (r.failures > 0) state = 'error'
    return { at: new Date(r.at * 1000).toISOString(), state }
  })
  return (
    <div
      className='space-y-3'
      role='img'
      aria-label={t('Request success and failure distribution')}
    >
      <div className='text-muted-foreground flex justify-between gap-3 text-xs'>
        <span>{t('Request history')}</span>
        <span>{t('Last hour · one cell every 5 minutes')}</span>
      </div>
      <ResultHistory
        history={history}
        kind='business'
        label={t('Request status history')}
        emptyLabel={t('No data')}
        slotCount={12}
        renderPoint={(point) => {
          const row = rows.find(
            (r) => new Date(r.at * 1000).toISOString() === point.at
          )
          if (!row) return t('No data')
          return (
            <>
              <div>
                {new Date((row.at - 300) * 1000).toLocaleTimeString(locale)}–
                {new Date(row.at * 1000).toLocaleTimeString(locale)}
              </div>
              <div>
                {t('Requests')}: {formatNumber(row.requests, locale)}
              </div>
              <div>
                {t('Failed requests')}: {formatNumber(row.failures, locale)}
              </div>
              <div>
                {t('Success rate')}:{' '}
                {formatNumber(100 - (row.failure_rate ?? 0), locale)}%
              </div>
            </>
          )
        }}
      />
      <div className='text-muted-foreground flex flex-wrap gap-4 text-xs'>
        {[
          ['bg-emerald-500', t('All requests succeeded')],
          ['bg-rose-500', t('All requests failed')],
          ['bg-amber-500', t('Some requests failed')],
          ['bg-muted', t('No requests')],
        ].map(([color, label]) => (
          <span key={label} className='flex items-center gap-1.5'>
            <span className={`${color} size-2 rounded-sm`} />
            {label}
          </span>
        ))}
      </div>
    </div>
  )
}
