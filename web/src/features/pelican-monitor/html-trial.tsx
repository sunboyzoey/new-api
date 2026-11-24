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

import { StatusBadge } from '@/components/status-badge'

import { DrawingCard, drawingPreviewClass } from './drawing-card'
import { useHtmlTrial, type TrialGroup } from './use-html-trial'

export function HtmlTrial(props: { group: TrialGroup }) {
  const { t } = useTranslation()
  const data = useHtmlTrial(props.group)
  if (!data) return null
  const title = t('One-time HTML drawing test')
  return (
    <DrawingCard
      model={data.model}
      group={data.group}
      timestamp={data.finished_at}
      status={
        <StatusBadge variant='info' copyable={false}>
          {title}
        </StatusBadge>
      }
    >
      <iframe
        title={title}
        className={drawingPreviewClass}
        sandbox='allow-scripts'
        referrerPolicy='no-referrer'
        loading='lazy'
        srcDoc={data.documentHtml}
      />
    </DrawingCard>
  )
}
