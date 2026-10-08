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
export type QualityPoint = {
  at: string
  state: 'passed' | 'failed' | 'error' | 'ungraded'
  score?: number
}
export function resultLabel(
  state: QualityPoint['state'],
  kind: string,
  t: (key: string) => string
) {
  if (state === 'passed') {
    return kind === 'pelican' ? t('SVG validated') : t('Passed')
  }
  if (state === 'failed') {
    return kind === 'pelican' ? t('Invalid SVG') : t('Failed')
  }
  if (state === 'error') return t('Test error')
  return t('Not graded')
}
