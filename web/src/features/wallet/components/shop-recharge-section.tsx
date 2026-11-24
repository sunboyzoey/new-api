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
import { ExternalLink, ShoppingBag } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import { IconBadge } from '@/components/ui/icon-badge'

export function ShopRechargeSection(props: { topupLink: string }) {
  const { t } = useTranslation()
  let url: URL
  try {
    url = new URL(props.topupLink)
  } catch {
    return null
  }
  if (
    !['https:', 'http:'].includes(url.protocol) ||
    url.username ||
    url.password
  ) {
    return null
  }
  return (
    <section
      aria-label={t('Recharge shop')}
      className='border-primary/25 bg-primary/5 space-y-3 rounded-xl border p-4 sm:p-5'
    >
      <div className='flex items-center gap-2'>
        <IconBadge tone='primary' size='xs'>
          <ShoppingBag />
        </IconBadge>
        <h3 className='font-semibold'>{t('Recharge shop')}</h3>
      </div>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Purchase a recharge code in the shop, then return here to redeem it.'
        )}
      </p>
      <Button
        size='lg'
        role='link'
        render={<a href={url.href} target='_blank' rel='noopener noreferrer' />}
        className='h-12 w-full gap-2 text-base font-semibold'
      >
        {t('Go to recharge payment')}
        <ExternalLink aria-hidden='true' />
      </Button>
    </section>
  )
}
