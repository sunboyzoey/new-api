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

import { isPublicPageDisabled } from '../nav-modules'

describe('disabled public pages', () => {
  const status = {
    HeaderNavModules: JSON.stringify({
      home: false,
      docs: false,
      rankings: { enabled: false, requireAuth: false },
    }),
  }
  it.each([
    '/',
    '/docs',
    '/docs/',
    '/docs/getting-started',
    '/rankings',
    '/rankings/',
  ])('blocks %s when its navigation module is disabled', (path) => {
    expect(isPublicPageDisabled(path, status)).toBe(true)
  })
  it.each([
    '/dashboard',
    '/sign-in',
    '/api/status',
    '/api/rankings',
    '/docs-other',
    '/rankings-other',
    '/pricing',
  ])('keeps %s accessible', (path) => {
    expect(isPublicPageDisabled(path, status)).toBe(false)
  })
  it.each(['/', '/docs', '/rankings'])(
    'preserves enabled and default access for %s',
    (path) => {
      expect(
        isPublicPageDisabled(path, {
          HeaderNavModules: JSON.stringify({
            home: true,
            docs: true,
            rankings: { enabled: true },
          }),
        })
      ).toBe(false)
      expect(isPublicPageDisabled(path, null)).toBe(false)
    }
  )
})
