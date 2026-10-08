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
import { useTranslation } from 'react-i18next'

import { LoadingState } from '@/components/loading-state'
import { svgDocument } from '@/features/channel-monitor/presentation'
import { toIntlLocale } from '@/i18n/languages'
import { api } from '@/lib/api'

export function DrawingsGallery(props: { group: string; model: string }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const query = useQuery({
    queryKey: ['group-drawings', props.group, props.model],
    queryFn: async () => {
      const { data } = await api.get<{
        success: boolean
        message?: string
        data: { at: string; svg: string; prompt?: string }[]
      }>('/api/group-monitor/drawings', {
        params: { group: props.group, model: props.model },
      })
      if (!data.success) throw new Error(data.message)
      return data.data
    },
    refetchInterval: 300000,
  })
  return (
    <section className='space-y-3 border-t pt-5'>
      <div className='flex flex-wrap justify-between gap-2'>
        <h3 className='text-sm font-medium'>
          {t('Latest 10 drawings')} · {props.model}
        </h3>
        <span className='text-muted-foreground text-xs'>
          {t('Successful validated drawings, newest first')}
        </span>
      </div>
      {query.isLoading && <LoadingState />}
      {query.isError && (
        <p role='alert' className='text-muted-foreground text-sm'>
          {t('Test results unavailable')}
        </p>
      )}
      <div className='grid gap-4 sm:grid-cols-2 lg:grid-cols-3'>
        {query.data?.slice(0, 10).map((drawing, index) => (
          <figure
            key={drawing.at}
            className='overflow-hidden rounded-xl border'
          >
            <iframe
              title={`${t('Pelican SVG preview')} ${index + 1}`}
              className='h-56 w-full border-0 bg-white'
              sandbox=''
              referrerPolicy='no-referrer'
              srcDoc={svgDocument(drawing.svg).replace(
                'max-height:400px',
                'max-height:200px'
              )}
            />
            <figcaption className='text-muted-foreground space-y-2 border-t px-3 py-3 text-xs'>
              <time dateTime={drawing.at}>
                {new Date(drawing.at).toLocaleString(locale)}
              </time>
              <div className='space-y-1'>
                <p className='text-foreground font-medium'>
                  {t('Drawing prompt')}
                </p>
                <p className='leading-relaxed break-words whitespace-pre-wrap'>
                  {drawing.prompt || t('Prompt not recorded')}
                </p>
              </div>
            </figcaption>
          </figure>
        ))}
      </div>
    </section>
  )
}
