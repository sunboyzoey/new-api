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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { CopyButton } from '@/components/copy-button'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { toIntlLocale } from '@/i18n/languages'
import { api } from '@/lib/api'
import { formatNumber } from '@/lib/format'
import { requireServerSuccess } from '@/lib/server-error-message'

import { SettingsSection } from '../components/settings-section'
import { useUpdateOption } from '../hooks/use-update-option'

type Invitation = {
  id: number
  expires_at: number
  used_at: number
  revoked_at: number
}
const queryKey = ['registration-invites']

export function RegistrationInvitesSection() {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const client = useQueryClient()
  const update = useUpdateOption()
  const [page, setPage] = useState(1)
  const [days, setDays] = useState(7)
  const [code, setCode] = useState('')
  const [revokeId, setRevokeId] = useState<number | null>(null)
  const query = useQuery({
    queryKey: [...queryKey, page],
    queryFn: async () => {
      const response = await api.get('/api/option/registration_invites', {
        params: { p: page },
      })
      return requireServerSuccess(response.data).data as {
        required: boolean
        items: Invitation[]
      }
    },
  })
  const create = useMutation({
    mutationFn: async () => {
      const response = await api.post('/api/option/registration_invites', {
        expires_at: Math.floor(Date.now() / 1000) + days * 86400,
      })
      return requireServerSuccess(response.data).data as { code: string }
    },
    onSuccess: (data) => {
      setCode(data.code)
      void client.invalidateQueries({ queryKey })
    },
  })
  const revoke = useMutation({
    mutationFn: async (id: number) =>
      requireServerSuccess(
        (await api.delete(`/api/option/registration_invites/${id}`)).data
      ),
    onSuccess: () => {
      setRevokeId(null)
      void client.invalidateQueries({ queryKey })
    },
  })
  return (
    <SettingsSection title={t('Registration invitations')}>
      <div className='flex items-center gap-3'>
        <Switch
          id='require-registration-invite'
          checked={query.data?.required ?? false}
          disabled={!query.data || update.isPending}
          onCheckedChange={(value) => {
            update.mutate(
              { key: 'RegistrationInviteRequired', value: String(value) },
              {
                onSuccess: () => {
                  void client.invalidateQueries({ queryKey })
                },
              }
            )
          }}
        />
        <Label htmlFor='require-registration-invite'>
          {t('Require an invitation for new accounts')}
        </Label>
      </div>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Invitations are single-use. Existing users can still sign in without an invitation.'
        )}
      </p>
      <div className='flex flex-wrap items-end gap-3'>
        <div className='space-y-2'>
          <Label htmlFor='invitation-days'>{t('Validity (days)')}</Label>
          <Input
            id='invitation-days'
            type='number'
            min={1}
            max={30}
            value={days}
            onChange={(event) => setDays(Number(event.target.value))}
          />
        </div>
        <Button
          disabled={
            create.isPending || !Number.isInteger(days) || days < 1 || days > 30
          }
          onClick={() => create.mutate()}
        >
          {t('Create invitation')}
        </Button>
      </div>
      {code && (
        <div className='space-y-2 rounded-md border p-3'>
          <p>{t('Copy this code now. It will not be shown again.')}</p>
          <div className='flex items-center gap-2'>
            <code className='break-all'>{code}</code>
            <CopyButton value={code} />
          </div>
          <Button variant='outline' onClick={() => setCode('')}>
            {t('Close')}
          </Button>
        </div>
      )}
      {query.isError && (
        <Button variant='outline' onClick={() => void query.refetch()}>
          {t('Retry')}
        </Button>
      )}
      {query.isPending && <p role='status'>{t('Loading...')}</p>}
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{t('ID')}</TableHead>
            <TableHead>{t('Expires at')}</TableHead>
            <TableHead>{t('Status')}</TableHead>
            <TableHead>{t('Actions')}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {query.data?.items.map((invite) => {
            let status = t('Available')
            if (invite.used_at) status = t('Used')
            else if (invite.revoked_at) status = t('Revoked')
            else if (invite.expires_at <= Date.now() / 1000)
              {status = t('Expired')}
            return (
              <TableRow key={invite.id}>
                <TableCell>{formatNumber(invite.id, locale)}</TableCell>
                <TableCell>
                  {new Date(invite.expires_at * 1000).toLocaleString(locale)}
                </TableCell>
                <TableCell>{status}</TableCell>
                <TableCell>
                  <Button
                    variant='outline'
                    disabled={
                      !!invite.used_at ||
                      !!invite.revoked_at ||
                      invite.expires_at <= Date.now() / 1000
                    }
                    onClick={() => setRevokeId(invite.id)}
                  >
                    {t('Revoke')}
                  </Button>
                </TableCell>
              </TableRow>
            )
          })}
        </TableBody>
      </Table>
      {query.data?.items.length === 0 && <p>{t('No invitations yet')}</p>}
      <div className='flex gap-2'>
        <Button
          variant='outline'
          disabled={page === 1}
          onClick={() => setPage(page - 1)}
        >
          {t('Previous')}
        </Button>
        <Button
          variant='outline'
          disabled={!query.data || query.data.items.length < 50}
          onClick={() => setPage(page + 1)}
        >
          {t('Next')}
        </Button>
      </div>
      <ConfirmDialog
        open={revokeId !== null}
        onOpenChange={(open) => {
          if (!open) setRevokeId(null)
        }}
        title={t('Revoke invitation')}
        desc={t('This invitation will no longer allow registration.')}
        destructive
        isLoading={revoke.isPending}
        handleConfirm={() => {
          if (revokeId !== null) revoke.mutate(revokeId)
        }}
      />
    </SettingsSection>
  )
}
