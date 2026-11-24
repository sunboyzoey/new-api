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
import { ExternalLink } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { SectionPageLayout } from '@/components/layout/components/section-page-layout'
import { Button } from '@/components/ui/button'

const REFUND_URL = 'https://weiwanga.top/refund'

export function Refund() {
  const { t } = useTranslation()

  return (
    <SectionPageLayout fixedContent>
      <SectionPageLayout.Title>{t('Refund service')}</SectionPageLayout.Title>
      <SectionPageLayout.Actions>
        <Button
          variant='outline'
          role='link'
          render={
            <a href={REFUND_URL} target='_blank' rel='noopener noreferrer' />
          }
        >
          <ExternalLink aria-hidden='true' />
          {t('Open in new window')}
        </Button>
      </SectionPageLayout.Actions>
      <SectionPageLayout.Content>
        <div className='flex h-full min-h-0 flex-col gap-3'>
          <p className='text-muted-foreground shrink-0 text-sm'>
            {t(
              'If the refund page does not display or sign-in fails, open it in a new window.'
            )}
          </p>
          {/* oxlint-disable react/iframe-missing-sandbox -- This fixed cross-origin service needs scripts and origin-bound storage; it cannot access the parent DOM. */}
          <iframe
            src={REFUND_URL}
            title={t('Refund service')}
            className='min-h-0 w-full flex-1 rounded-lg border'
            referrerPolicy='no-referrer'
            sandbox='allow-scripts allow-same-origin allow-forms allow-popups allow-popups-to-escape-sandbox'
            allow='clipboard-write'
          />
          {/* oxlint-enable react/iframe-missing-sandbox */}
        </div>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
