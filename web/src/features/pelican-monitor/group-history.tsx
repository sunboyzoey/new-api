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
import { Bird } from 'lucide-react'
import { useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { StatusBadge } from '@/components/status-badge'
import {
  Accordion,
  AccordionContent,
  AccordionItem,
  AccordionTrigger,
} from '@/components/ui/accordion'
import { Button } from '@/components/ui/button'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'

import { getPelicanResults, type GroupSummary, type PelicanResult } from './api'
import { DrawingCard, drawingPreviewClass } from './drawing-card'

export function TestStatus(props: { status?: PelicanResult['status'] }) {
  const { t } = useTranslation()
  const states = {
    failed: { label: t('Failed'), variant: 'danger' },
    running: { label: t('Running'), variant: 'info' },
    pending_review: { label: t('Drawing valid'), variant: 'success' },
    untested: { label: t('No test results yet'), variant: 'neutral' },
  } as const
  return (
    <StatusBadge
      {...states[props.status ?? 'untested']}
      copyable={false}
      showDot
    />
  )
}

export function GroupTestDisclosure({
  group,
  defaultExpanded = false,
  children,
}: {
  group: GroupSummary
  defaultExpanded?: boolean
  children?: ReactNode
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const [expanded, setExpanded] = useState<string[]>(() =>
    defaultExpanded ? [group.id] : []
  )
  const results = new Map(group.model_results.map((row) => [row.model, row]))
  return (
    <Accordion
      value={expanded}
      onValueChange={setExpanded}
      className='border-t'
    >
      <AccordionItem value={group.id}>
        <AccordionTrigger className='items-center rounded-none px-4 py-4 hover:no-underline sm:px-6'>
          <span className='flex min-w-0 flex-1 flex-col items-start gap-2'>
            <span className='flex flex-wrap items-center gap-x-2 gap-y-1 text-left'>
              <Bird aria-hidden='true' className='size-4 shrink-0' />
              <span className='font-semibold'>{t('Intelligence test')}</span>
              <span className='text-muted-foreground text-xs font-normal'>
                {t('Pelican bicycle test results')}
              </span>
            </span>
            {group.models.map((model) => {
              const result = results.get(model)
              return (
                <span
                  key={model}
                  className='flex flex-wrap items-center gap-x-2 gap-y-1 text-left'
                >
                  <span className='font-mono text-xs wrap-anywhere'>
                    {model}
                  </span>
                  <TestStatus status={result?.status} />
                  {result && (
                    <time
                      dateTime={new Date(result.started_at).toISOString()}
                      className='text-muted-foreground text-xs font-normal'
                    >
                      {t('Last Tested')} ·{' '}
                      {new Date(result.started_at).toLocaleString(locale)}
                    </time>
                  )}
                </span>
              )
            })}
          </span>
        </AccordionTrigger>
        <AccordionContent className='px-4 pb-5 sm:px-6'>
          {expanded.includes(group.id) && (
            <>
              <p className='text-muted-foreground mb-3 text-xs'>
                {t(
                  'Drawing validation checks structure and safety. Drawing quality requires review.'
                )}
              </p>
              {children ?? (
                <GroupHistory
                  key={group.group}
                  target={group.id}
                  group={group.group}
                />
              )}
            </>
          )}
        </AccordionContent>
      </AccordionItem>
    </Accordion>
  )
}

function GroupHistory(props: { target: string; group: string }) {
  const { t } = useTranslation()
  const [cursors, setCursors] = useState<number[]>([0])
  const before = cursors.at(-1) ?? 0
  const query = useQuery({
    queryKey: ['pelican-results', props.target, props.group, before],
    queryFn: ({ signal }) => getPelicanResults(before, signal, props.target),
    refetchInterval: before === 0 ? 30_000 : false,
  })
  if (query.isPending) return <LoadingState />
  if (query.isError) {
    return (
      <ErrorState
        onRetry={() => {
          if (before > 0) setCursors([0])
          else void query.refetch()
        }}
      />
    )
  }
  const rows = query.data.items
  return (
    <div className='@container space-y-4'>
      {rows.length === 0 ? (
        <EmptyState title={t('No test results yet')} />
      ) : (
        <PelicanResultGrid>
          {rows.map((row) => (
            <PelicanResultCard key={row.id} result={row} />
          ))}
        </PelicanResultGrid>
      )}
      {(cursors.length > 1 || query.data.has_more) && (
        <nav
          aria-label={t('Pagination')}
          className='flex items-center justify-between gap-3'
        >
          <Button
            variant='outline'
            disabled={cursors.length === 1}
            onClick={() => setCursors((items) => items.slice(0, -1))}
          >
            {t('Previous')}
          </Button>
          <Button
            variant='outline'
            disabled={!query.data.has_more}
            onClick={() => {
              const last = rows.at(-1)
              if (last) setCursors((items) => [...items, last.id])
            }}
          >
            {t('Next')}
          </Button>
        </nav>
      )}
    </div>
  )
}

export function PelicanResultGrid(props: { children: ReactNode }) {
  return (
    <div className='grid grid-cols-[repeat(auto-fill,minmax(min(100%,14rem),1fr))] gap-3'>
      {props.children}
    </div>
  )
}

const previewHead = '<!DOCTYPE html><html><head>'
const previewPolicy = `<meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src data:; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'">`

export function PelicanResultCard(props: { result: PelicanResult }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const row = props.result
  const errors: Record<string, string> = {
    upstream_error: t('Upstream request failed'),
    credential_error: t('Monitor credential unavailable'),
    group_key_mismatch: t('API key group does not match'),
    timeout: t('Test timed out'),
    interrupted: t('Test interrupted'),
    unsupported_model: t('This model does not support the drawing test'),
    invalid_html: t(
      'The response did not contain a complete valid HTML drawing'
    ),
    unsafe_html: t('The HTML drawing contains unsupported or unsafe content'),
    invalid_svg: t('The response did not contain a complete valid SVG'),
    missing_animation: t('The drawing is missing the requested animation'),
    unsafe_svg: t('The SVG contains unsupported or unsafe content'),
    output_too_large: t('The drawing exceeds the output limit'),
    truncated_output: t('The model output was truncated'),
  }
  // Only the normalized backend document can follow our iframe policy.
  const documentHtml = row.html?.startsWith(previewHead)
    ? previewHead + previewPolicy + row.html.slice(previewHead.length)
    : undefined
  return (
    <DrawingCard
      model={row.model}
      group={row.group}
      timestamp={row.started_at}
      status={
        <>
          <TestStatus status={row.status} />
          {row.finished_at > 0 && (
            <span className='text-muted-foreground text-xs'>
              {formatNumber(row.duration_ms, locale)} ms
            </span>
          )}
        </>
      }
    >
      {row.error && (
        <p role='alert' className='text-destructive text-sm'>
          {errors[row.error] ?? t('Upstream request failed')}
        </p>
      )}
      {row.status === 'failed' && (row.http_status ?? 0) > 0 && (
        <p className='text-muted-foreground text-xs'>HTTP {row.http_status}</p>
      )}
      {documentHtml && (
        <iframe
          title={t('Pelican bicycle HTML animation')}
          sandbox=''
          referrerPolicy='no-referrer'
          loading='lazy'
          srcDoc={documentHtml}
          className={drawingPreviewClass}
        />
      )}
      {!documentHtml && row.svg && (
        <img
          src={`data:image/svg+xml;charset=utf-8,${encodeURIComponent(row.svg)}`}
          alt={t('Pelican bicycle SVG drawing')}
          loading='lazy'
          decoding='async'
          className={drawingPreviewClass}
        />
      )}
    </DrawingCard>
  )
}
