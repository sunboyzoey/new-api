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
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useFieldArray, useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { z } from 'zod'

import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { PasswordInput } from '@/components/password-input'
import { Button } from '@/components/ui/button'
import { Card, CardHeader, CardTitle, CardContent } from '@/components/ui/card'
import {
  Form,
  FormField,
  FormItem,
  FormLabel,
  FormControl,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'

import { getMonitorConfig, saveMonitorConfig, type MonitorConfig } from './api'

const schema = z.object({
  enabled: z.boolean(),
  targets: z
    .array(
      z.object({
        id: z.string(),
        group: z.string().trim().min(1).max(128),
        base_url: z
          .url()
          .refine((value) => value.startsWith('https://'), 'HTTPS is required'),
        api_key: z.string(),
        has_api_key: z.boolean(),
        models_text: z.string().trim().min(1),
        endpoint: z.enum(['chat/completions', 'responses']),
        reasoning_effort: z.enum(['', 'medium']),
        enabled: z.boolean(),
      })
    )
    .max(50),
})
type ConfigForm = z.infer<typeof schema>

export function MonitorConfiguration() {
  const query = useQuery({
    queryKey: ['pelican-config'],
    staleTime: Infinity,
    refetchOnWindowFocus: false,
    queryFn: ({ signal }) => getMonitorConfig(signal),
  })
  if (query.isPending) return <LoadingState />
  if (query.isError) return <ErrorState onRetry={() => void query.refetch()} />
  return <MonitorConfigForm key={query.dataUpdatedAt} config={query.data} />
}

function MonitorConfigForm(props: { config: MonitorConfig }) {
  const { t } = useTranslation()
  const client = useQueryClient()
  const form = useForm<ConfigForm>({
    resolver: zodResolver(schema),
    defaultValues: {
      enabled: props.config.enabled,
      targets: props.config.targets.map((row) => ({
        ...row,
        api_key: '',
        models_text: row.models.join(', '),
        reasoning_effort: row.reasoning_effort ?? '',
      })),
    },
  })
  const fields = useFieldArray({
    control: form.control,
    name: 'targets',
    keyName: 'formKey',
  })
  const save = useMutation({
    mutationFn: saveMonitorConfig,
    onSuccess: async () => {
      for (let index = 0; index < form.getValues('targets').length; index++) {
        form.setValue(`targets.${index}.api_key`, '')
      }
      toast.success(t('Saved successfully'))
      await Promise.all([
        client.invalidateQueries({ queryKey: ['pelican-config'] }),
        client.invalidateQueries({ queryKey: ['pelican-groups'] }),
      ])
    },
  })
  function submit(values: ConfigForm) {
    save.mutate({
      enabled: values.enabled,
      targets: values.targets.map((row) => ({
        id: row.id,
        group: row.group,
        base_url: row.base_url,
        api_key: row.api_key,
        has_api_key: row.has_api_key,
        models: row.models_text
          .split(/[,，\n]/)
          .map((name) => name.trim())
          .filter(Boolean),
        endpoint: row.endpoint,
        reasoning_effort: row.reasoning_effort || undefined,
        enabled: row.enabled,
      })),
    })
  }
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('Group test configuration')}</CardTitle>
      </CardHeader>
      <CardContent className='space-y-4'>
        <p className='text-muted-foreground text-sm'>
          {t(
            'Use one fixed-group API key per group. Models are tested exactly as configured. Requests consume gateway quota.'
          )}
        </p>
        <p className='text-muted-foreground text-sm'>
          {t(
            'Local group bindings are verified. For remote sites, ensure the key belongs to the named group and disable cross-group retry.'
          )}
        </p>
        <p className='text-muted-foreground text-sm'>
          {t(
            'Keys are encrypted on the server. Leave a saved key blank to keep it; changing site or group requires entering a key again.'
          )}
        </p>
        <Form {...form}>
          <form onSubmit={form.handleSubmit(submit)} className='space-y-5'>
            <fieldset disabled={save.isPending} className='space-y-5'>
              <FormField
                control={form.control}
                name='enabled'
                render={({ field }) => (
                  <FormItem className='flex items-center gap-3'>
                    <FormControl>
                      <Switch
                        checked={field.value}
                        onCheckedChange={field.onChange}
                      />
                    </FormControl>
                    <FormLabel>{t('Enable scheduled group tests')}</FormLabel>
                  </FormItem>
                )}
              />
              {fields.fields.map((row, index) => (
                <div
                  key={row.formKey}
                  className='space-y-4 rounded-lg border p-4'
                >
                  <div className='grid gap-4 md:grid-cols-2'>
                    <FormField
                      control={form.control}
                      name={`targets.${index}.group`}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('Group')}</FormLabel>
                          <FormControl>
                            <Input {...field} />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`targets.${index}.base_url`}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('Site URL')}</FormLabel>
                          <FormControl>
                            <Input
                              {...field}
                              type='url'
                              placeholder='https://example.com'
                            />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`targets.${index}.api_key`}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('Group API key')}</FormLabel>
                          <FormControl>
                            <PasswordInput
                              {...field}
                              autoComplete='new-password'
                              placeholder={
                                row.has_api_key
                                  ? t('Saved key · leave blank to keep')
                                  : 'sk-...'
                              }
                            />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`targets.${index}.models_text`}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>
                            {t('Test models (comma separated)')}
                          </FormLabel>
                          <FormControl>
                            <Input {...field} />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                  </div>
                  <div className='flex flex-wrap items-center gap-5'>
                    <FormField
                      control={form.control}
                      name={`targets.${index}.enabled`}
                      render={({ field }) => (
                        <FormItem className='flex items-center gap-2'>
                          <FormControl>
                            <Switch
                              checked={field.value}
                              onCheckedChange={field.onChange}
                            />
                          </FormControl>
                          <FormLabel>{t('Enable this group')}</FormLabel>
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`targets.${index}.endpoint`}
                      render={({ field }) => (
                        <FormItem className='flex items-center gap-2'>
                          <FormControl>
                            <Switch
                              checked={field.value === 'responses'}
                              onCheckedChange={(checked) =>
                                field.onChange(
                                  checked ? 'responses' : 'chat/completions'
                                )
                              }
                            />
                          </FormControl>
                          <FormLabel>{t('Use Responses API')}</FormLabel>
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`targets.${index}.reasoning_effort`}
                      render={({ field }) => (
                        <FormItem className='flex items-center gap-2'>
                          <FormControl>
                            <Switch
                              checked={field.value === 'medium'}
                              onCheckedChange={(checked) =>
                                field.onChange(checked ? 'medium' : '')
                              }
                            />
                          </FormControl>
                          <FormLabel>
                            {t('Use medium reasoning effort')}
                          </FormLabel>
                        </FormItem>
                      )}
                    />
                    <Button
                      type='button'
                      variant='outline'
                      onClick={() => fields.remove(index)}
                    >
                      {t('Remove')}
                    </Button>
                  </div>
                </div>
              ))}
              <div className='flex flex-wrap gap-3'>
                <Button
                  type='button'
                  variant='outline'
                  disabled={fields.fields.length >= 50}
                  onClick={() =>
                    fields.append({
                      id: '',
                      group: '',
                      base_url: window.location.origin,
                      api_key: '',
                      has_api_key: false,
                      models_text: '',
                      endpoint: 'chat/completions',
                      reasoning_effort: '',
                      enabled: true,
                    })
                  }
                >
                  {t('Add monitoring group')}
                </Button>
                <Button type='submit' disabled={save.isPending}>
                  {t('Save')}
                </Button>
              </div>
            </fieldset>
            {save.isError && (
              <p role='alert' className='text-destructive text-sm'>
                {t('Configuration could not be saved')}
              </p>
            )}
          </form>
        </Form>
      </CardContent>
    </Card>
  )
}
