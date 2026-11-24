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
import i18next from 'i18next'
import { describe, expect, it } from 'vitest'

import {
  PLAN_FORM_DEFAULTS,
  formValuesToPlanPayload,
  getPlanFormSchema,
} from '../plan-form'

describe('Public benefit plan configuration', () => {
  it.each(['daily', 'weekly', 'monthly'] as const)(
    'keeps %s activity limits independent of lifetime and quota reset',
    (cycle) => {
      const values = getPlanFormSchema(i18next.t).parse({
        ...PLAN_FORM_DEFAULTS,
        title: 'Community',
        price_amount: 0,
        total_amount: 1,
        daily_claim_limit: 100,
        claim_cycle: cycle,
        max_claims_per_cycle: 1,
        max_purchase_per_user: 0,
        quota_reset_period: 'never',
      })
      expect(formValuesToPlanPayload(values).plan).toMatchObject({
        price_amount: 0,
        total_amount: 500000,
        daily_claim_limit: 100,
        claim_cycle: cycle,
        max_claims_per_cycle: 1,
        max_purchase_per_user: 0,
        quota_reset_period: 'never',
      })
    }
  )
  it('retains total quota zero as unlimited while allowing free price zero', () => {
    const values = getPlanFormSchema(i18next.t).parse({
      ...PLAN_FORM_DEFAULTS,
      title: 'Unlimited',
      price_amount: 0,
      total_amount: 0,
    })
    expect(formValuesToPlanPayload(values).plan).toMatchObject({
      price_amount: 0,
      total_amount: 0,
    })
  })
  it('rejects a free custom duration of zero', () => {
    const result = getPlanFormSchema(i18next.t).safeParse({
      ...PLAN_FORM_DEFAULTS,
      title: 'Invalid',
      duration_unit: 'custom',
      custom_seconds: 0,
    })
    expect(result.success).toBe(false)
    if (!result.success) {
      expect(result.error.issues).toEqual(
        expect.arrayContaining([
          expect.objectContaining({ path: ['custom_seconds'] }),
        ])
      )
    }
  })
  it.each([-1, 0.5])('rejects invalid daily stock %s', (limit) => {
    expect(
      getPlanFormSchema(i18next.t).safeParse({
        ...PLAN_FORM_DEFAULTS,
        title: 'Invalid',
        daily_claim_limit: limit,
      }).success
    ).toBe(false)
  })
})
