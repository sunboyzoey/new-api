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
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from '@/components/ui/popover'
import { formatTimestampToDate } from '@/lib/format'

import type { CodexTicketObservation } from '../types'
import { DetailRow } from './dialogs/log-detail-layout'

export function CodexTicketDetails({
  ticket,
}: {
  ticket: CodexTicketObservation
}) {
  const { t } = useTranslation()
  const summary = ticket.returned
  return (
    <div className='min-w-0 space-y-2 [&>div]:grid-cols-[auto_minmax(0,1fr)]'>
      <DetailRow
        label={t('Ticket(Returned ticket):', { nsSeparator: false })}
        value={summary ? String(summary.length) : '—'}
        mono
      />
      <DetailRow
        label={t('Fingerprint:', { nsSeparator: false })}
        mono
        value={
          summary ? (
            <span className='inline-flex max-w-full items-start gap-1'>
              <span className='min-w-0 break-all'>{summary.fingerprint}</span>
              <CopyButton value={summary.fingerprint} className='size-5' />
            </span>
          ) : (
            '—'
          )
        }
      />
      <DetailRow
        label={t('Issued at:', { nsSeparator: false })}
        value={
          summary?.issued_at ? formatTimestampToDate(summary.issued_at) : '—'
        }
      />
    </div>
  )
}

export function CodexTicketBadge({
  ticket,
}: {
  ticket?: CodexTicketObservation
}) {
  const { t } = useTranslation()
  if (!ticket) return <span className='text-muted-foreground'>—</span>
  const summary = ticket.returned
  const label = summary ? String(summary.length) : '—'
  return (
    <Popover>
      <PopoverTrigger
        render={
          <Button
            variant='ghost'
            className='h-auto max-w-full p-0'
            aria-label={`${t('Ticket')}: ${label}`}
          />
        }
      >
        <StatusBadge
          label={label}
          variant={summary ? 'info' : 'neutral'}
          copyable={false}
          className='max-w-full font-mono text-xs'
        />
      </PopoverTrigger>
      <PopoverContent
        className='w-80 max-w-[calc(100vw-2rem)]'
        align='start'
        aria-label={t('Ticket')}
      >
        <CodexTicketDetails ticket={ticket} />
      </PopoverContent>
    </Popover>
  )
}
