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
import { describe, expect, it } from 'vitest'

import { channelSchema } from '../../types'
import {
  CHANNEL_FORM_DEFAULT_VALUES,
  buildSettingJSON,
  channelFormSchema,
  transformChannelToFormDefaults,
} from '../channel-form'
import { isUnlimitedUpstreamBalance } from '../upstream-balance'

describe('upstream balance settings', () => {
  it.each(['newapi', 'sub2api'] as const)(
    'preserves %s when saving and reopening a channel',
    (system) => {
      const setting = buildSettingJSON({
        ...CHANNEL_FORM_DEFAULT_VALUES,
        balance_system: system,
      })
      expect(JSON.parse(setting).balance_system).toBe(system)
      const channel = channelSchema.parse({
        id: 1,
        name: 'Upstream',
        key: '',
        created_time: 0,
        test_time: 0,
        response_time: 0,
        type: 1,
        status: 1,
        models: 'm',
        group: 'default',
        balance_updated_time: 0,
        setting,
      })
      expect(transformChannelToFormDefaults(channel).balance_system).toBe(
        system
      )
    }
  )
  it('rejects unsupported systems', () => {
    const result = channelFormSchema.safeParse({
      ...CHANNEL_FORM_DEFAULT_VALUES,
      name: 'upstream',
      key: 'test',
      models: 'm',
      balance_system: 'unknown',
    })
    expect(result.success).toBe(false)
  })
  it('keeps the provider default when the setting is cleared', () => {
    expect(
      JSON.parse(
        buildSettingJSON({ ...CHANNEL_FORM_DEFAULT_VALUES, balance_system: '' })
      ).balance_system
    ).toBeUndefined()
  })
  it('displays unlimited only for upstream queries with the sentinel balance', () => {
    expect(isUnlimitedUpstreamBalance({ type: 60, balance: -1 })).toBe(true)
    expect(isUnlimitedUpstreamBalance({ type: 59, balance: -1 })).toBe(true)
    expect(
      isUnlimitedUpstreamBalance({
        type: 1,
        setting: '{"balance_system":"sub2api"}',
        balance: -1,
      })
    ).toBe(true)
    expect(isUnlimitedUpstreamBalance({ type: 1, balance: -1 })).toBe(false)
    expect(isUnlimitedUpstreamBalance({ type: 60, balance: 0 })).toBe(false)
    expect(
      isUnlimitedUpstreamBalance({ type: 1, setting: 'invalid', balance: -1 })
    ).toBe(false)
  })
})
