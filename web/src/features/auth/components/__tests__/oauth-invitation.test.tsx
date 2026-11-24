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
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { getServerErrorMessageKey } from '@/lib/server-error-message'

import { OAuthProviders } from '../oauth-providers'

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

test('login never asks for a registration invitation when invitation-only registration is enabled', () => {
  render(
    <OAuthProviders
      status={{
        registration_invite_required: true,
        github_oauth: true,
        github_client_id: 'client',
      }}
    />
  )
  expect(
    screen.getByRole('button', { name: /Continue with GitHub/ })
  ).toBeEnabled()
  expect(
    screen.queryByLabelText('Registration invitation')
  ).not.toBeInTheDocument()
})

test.each([undefined, 'registration-secret'])(
  'OAuth sends only the invitation supplied by the registration form: %s',
  async (invitation) => {
    vi.spyOn(window, 'localStorage', 'get').mockReturnValue(
      window.sessionStorage
    )
    const post = vi.spyOn(api, 'post').mockImplementation(async (url) => {
      if (url === '/api/user/auth/logout') return { data: { success: true } }
      if (url === '/api/oauth/state') {
        return {
          data: { success: true, data: { flow_token: 'invitation-test-flow' } },
        }
      }
      throw new Error('Unexpected API path')
    })
    vi.spyOn(window, 'open').mockReturnValue(null)
    const user = userEvent.setup()
    render(
      <OAuthProviders
        status={{
          registration_invite_required: true,
          github_oauth: true,
          github_client_id: 'client',
        }}
        registrationInvite={invitation}
      />
    )
    await user.click(
      screen.getByRole('button', { name: /Continue with GitHub/ })
    )
    expect(post).toHaveBeenCalledWith(
      '/api/oauth/state',
      expect.objectContaining({ registration_invite: invitation }),
      expect.anything()
    )
  }
)

test('invitation failures tell new users to enter their invitation on the sign-up page', () => {
  expect(
    getServerErrorMessageKey({ code: 'REGISTRATION_INVITE_INVALID' })
  ).toBe(
    'Registration requires a valid invitation. Go to the sign-up page and enter your invitation.'
  )
})
