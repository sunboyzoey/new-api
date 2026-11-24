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
import type { CacheTrendPoint, GroupRatePoint } from './api'

export type Hours = 1 | 6 | 24
export type RateSample = CacheTrendPoint | GroupRatePoint
export const intervalMinutes = { 1: 5, 6: 15, 24: 30 } as const

export function aggregateGroupRates(
  points: readonly RateSample[],
  finishedAt: number,
  hours: Hours
) {
  const step = intervalMinutes[hours] * 60
  const start = finishedAt - hours * 3600
  const bins = Array.from({ length: (hours * 3600) / step }, (_, i) => ({
    timestamp: start + i * step,
    cacheInput: 0,
    cacheRead: 0,
    requests: 0,
    errors: 0,
  }))
  let cacheInput = 0
  let cacheRead = 0
  for (const point of points) {
    const index = Math.floor((point.timestamp - start) / step)
    const bin = bins[index]
    if (!bin) continue
    if (point.cache_samples > 0 && point.cache_input_tokens > 0) {
      bin.cacheInput += point.cache_input_tokens
      bin.cacheRead += point.cache_read_tokens
      cacheInput += point.cache_input_tokens
      cacheRead += point.cache_read_tokens
    }
    if ('error_rate' in point && point.error_rate != null) {
      bin.requests += point.requests
      bin.errors += point.errors
    }
  }
  return {
    cacheHitRate: cacheInput > 0 ? (100 * cacheRead) / cacheInput : null,
    points: bins.map((bin) => ({
      timestamp: bin.timestamp,
      cache_hit_rate:
        bin.cacheInput > 0 ? (100 * bin.cacheRead) / bin.cacheInput : null,
      error_rate: bin.requests > 0 ? (100 * bin.errors) / bin.requests : null,
    })),
  }
}
