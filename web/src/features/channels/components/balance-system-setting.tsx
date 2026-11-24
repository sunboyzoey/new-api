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
import type { Control } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import {
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

import type { ChannelFormValues } from '../lib/channel-form'

export function BalanceSystemSetting(props: {
  control: Control<ChannelFormValues>
  disabled?: boolean
}) {
  const { t } = useTranslation()
  const items = [
    { value: 'default', label: t('Use channel type') },
    { value: 'newapi', label: 'New API' },
    { value: 'sub2api', label: 'Sub2API' },
  ]
  return (
    <FormField
      control={props.control}
      name='balance_system'
      render={({ field }) => (
        <FormItem>
          <FormLabel>{t('Balance query system')}</FormLabel>
          <Select
            items={items}
            disabled={props.disabled}
            value={field.value || 'default'}
            onValueChange={(value) =>
              field.onChange(value === 'default' ? '' : value)
            }
          >
            <FormControl>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
            </FormControl>
            <SelectContent alignItemWithTrigger={false}>
              <SelectGroup>
                {items.map((item) => (
                  <SelectItem key={item.value} value={item.value}>
                    {item.label}
                  </SelectItem>
                ))}
              </SelectGroup>
            </SelectContent>
          </Select>
          <FormDescription>
            {t(
              'New API and Sub2API balances refresh every 10 minutes using the existing API key. Shows available key, wallet, or subscription quota; multi-key channels are not supported.'
            )}
          </FormDescription>
          <FormMessage />
        </FormItem>
      )}
    />
  )
}
