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
import { api } from '@/lib/api'
import { requireServerSuccess } from '@/lib/server-error-message'

export type PelicanResult = {
  id: number
  target_id: string
  group: string
  model: string
  started_at: number
  finished_at: number
  duration_ms: number
  status: 'running' | 'failed' | 'pending_review'
  error: string
  http_status?: number
  html?: string
  svg?: string
}
export type PelicanPage = {
  items: PelicanResult[]
  has_more: boolean
  enabled: boolean
  interval_minutes: number
}
export async function getPelicanResults(
  before: number,
  signal?: AbortSignal,
  target?: string
): Promise<PelicanPage> {
  const response = await api.get<{ success: boolean; data: PelicanPage }>(
    '/api/pelican/results',
    { params: { before, target }, signal }
  )
  return requireServerSuccess(response.data).data
}
export type CacheHealth = {
  group: string
  channel_id: number
  model: string
  requests: number
  errors: number
  input_tokens: number
  output_tokens: number
  cache_read_tokens: number
  cache_write_tokens: number
  cache_samples: number
  cache_input_tokens: number
  cache_hit_rate: number | null
}
export type GroupRatePoint = {
  timestamp: number
  requests: number
  errors: number
  cache_input_tokens: number
  cache_read_tokens: number
  cache_samples: number
  cache_hit_rate: number | null
  error_rate: number | null
}
export type CacheTrendPoint = Omit<GroupRatePoint, 'error_rate'> & {
  input_tokens: number
  output_tokens: number
  cache_write_tokens: number
}
export type CacheTrend = { group: string; points: CacheTrendPoint[] }
export type CacheReport = {
  trends?: CacheTrend[]
  bucket_seconds?: number

  groups: CacheHealth[]
  channels: CacheHealth[]
  started_at: number
  finished_at: number
  truncated: boolean
}
export async function getCacheHealth(
  signal?: AbortSignal
): Promise<CacheReport> {
  const response = await api.get<{ success: boolean; data: CacheReport }>(
    '/api/group-health/cache',
    { signal }
  )
  return requireServerSuccess(response.data).data
}

export type GroupTarget = {
  id: string
  group: string
  base_url: string
  api_key?: string
  has_api_key: boolean
  models: string[]
  endpoint: 'chat/completions' | 'responses'
  reasoning_effort?: '' | 'medium'
  enabled: boolean
}
export type MonitorConfig = { enabled: boolean; targets: GroupTarget[] }
export type GroupSummary = {
  id: string
  group: string
  models: string[]
  enabled: boolean
  group_verified: boolean
  latest: PelicanResult | null
  model_results: PelicanResult[]
  cache: CacheHealth
  error_rate: number | null
  trend: GroupRatePoint[]
  finished_at: number
  cache_source: 'local' | 'probe' | 'unavailable'
  truncated: boolean
}
export async function getPelicanGroups(signal?: AbortSignal): Promise<{
  groups: GroupSummary[]
  enabled: boolean
  interval_minutes: number
}> {
  return requireServerSuccess(
    (await api.get('/api/pelican/groups', { signal })).data
  ).data
}
export async function startGroupTest(targetId: string) {
  const response = await api.post<{
    success: boolean
    data: { created: boolean; task_id: string }
  }>(`/api/group-health/targets/${encodeURIComponent(targetId)}/test`)
  return requireServerSuccess(response.data).data
}
export async function getMonitorConfig(
  signal?: AbortSignal
): Promise<MonitorConfig> {
  return requireServerSuccess(
    (await api.get('/api/group-health/config', { signal })).data
  ).data
}
export async function saveMonitorConfig(config: MonitorConfig): Promise<void> {
  requireServerSuccess((await api.put('/api/group-health/config', config)).data)
}
