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

import type { GroupSummary } from './api'
import { getHtmlTrial, reviewedHtmlTrial } from './html-trial-document'

export type TrialGroup = Pick<GroupSummary, 'id' | 'group' | 'models'>

export function useHtmlTrial(group?: TrialGroup) {
  const matches =
    group?.id === reviewedHtmlTrial.target_id &&
    group.group === reviewedHtmlTrial.group &&
    group.models.includes(reviewedHtmlTrial.model)
  const { data } = useQuery({
    queryKey: ['pelican-html-trial', reviewedHtmlTrial.documentSha256],
    queryFn: ({ signal }) => getHtmlTrial(reviewedHtmlTrial, signal),
    enabled: matches,
    staleTime: Infinity,
    retry: false,
    meta: { errorToast: false },
  })
  return matches ? data : undefined
}
