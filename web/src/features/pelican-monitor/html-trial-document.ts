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
import { z } from 'zod'

const recordSchema = z.strictObject({
  schema_version: z.literal(1),
  target_id: z.string().min(1).max(128),
  group: z.string().min(1).max(128),
  model: z.string().min(1).max(256),
  finished_at: z.number().int().positive().max(8_640_000_000_000_000),
  html: z.string().min(1).max(1_048_576),
})

// Reviewed release data. Replacing the downloaded JSON cannot authorize a new
// document, move this result to another group, or grant a script permission.
export const reviewedHtmlTrial = {
  target_id: 'AmXfK8KAH0uraBgvpnxwiMuo',
  group: 'GPT PRO (退款流)',
  model: 'gpt-6.2-astra',
  finished_at: 1795446187068,
  documentSha256:
    'f434f753c3036baa8e97bbac69f7f4440a76fa1a197e14c209fb9d1d77318c38',
  scriptSha256: 'rGZqgy3VZ7+m396nuDzuidETQKjeB/NtQAirKMbfC5U=',
}

// The reviewed document opens with a DOCTYPE, an optional root language, and
// a head. No parser-active markup is allowed ahead of the injected policy.
const headStart =
  /^\s*<!doctype\s+html\s*>\s*<html(?:\s+lang=(?:"[a-z0-9-]+"|'[a-z0-9-]+'))?\s*>\s*<head\s*>/i
const blockedElements =
  'a, area, base, embed, form, frame, frameset, iframe, link, meta[http-equiv], object, portal, template'

export async function getHtmlTrial(
  review: typeof reviewedHtmlTrial,
  signal: AbortSignal
) {
  if (!globalThis.crypto?.subtle) return null
  const response = await fetch(
    `/static/monitor/pelican-html-trial.json?v=${review.documentSha256}`,
    {
      signal,
      credentials: 'omit',
      mode: 'same-origin',
      redirect: 'error',
      referrerPolicy: 'no-referrer',
    }
  )
  if (
    !response.ok ||
    !response.headers.get('content-type')?.startsWith('application/json')
  ) {
    return null
  }
  const parsed = recordSchema.safeParse(await response.json())
  if (!parsed.success) return null
  const { html, ...record } = parsed.data
  if (
    record.target_id !== review.target_id ||
    record.group !== review.group ||
    record.model !== review.model ||
    record.finished_at !== review.finished_at
  ) {
    return null
  }
  const encoder = new TextEncoder()
  const hash = new Uint8Array(
    await crypto.subtle.digest('SHA-256', encoder.encode(html))
  )
  if (
    Array.from(hash, (byte) => byte.toString(16).padStart(2, '0')).join('') !==
    review.documentSha256
  ) {
    return null
  }
  const prologue = headStart.exec(html)
  if (!prologue) return null

  // A detached template has inert contents. Its nodes never enter the host page.
  const template = document.createElement('template')
  template.innerHTML = html
  if (template.content.querySelector(blockedElements)) return null
  for (const element of template.content.querySelectorAll('*')) {
    for (const attr of element.attributes) {
      if (
        attr.name.startsWith('on') ||
        ['src', 'srcset', 'formaction', 'ping'].includes(attr.name)
      ) {
        return null
      }
      if (
        attr.localName === 'href' &&
        !(element.localName === 'use' && /^#[\w.-]+$/.test(attr.value))
      ) {
        return null
      }
    }
  }
  const scripts = template.content.querySelectorAll('script')
  if (scripts.length !== 1 || scripts[0].hasAttributes()) return null
  const scriptHash = new Uint8Array(
    await crypto.subtle.digest(
      'SHA-256',
      encoder.encode(scripts[0].textContent ?? '')
    )
  )
  if (btoa(String.fromCharCode(...scriptHash)) !== review.scriptSha256) {
    return null
  }
  const policy = [
    "default-src 'none'",
    "style-src 'unsafe-inline'",
    `script-src 'sha256-${review.scriptSha256}'`,
    "script-src-attr 'none'",
    "base-uri 'none'",
    "form-action 'none'",
    "object-src 'none'",
    "frame-src 'none'",
    "connect-src 'none'",
  ].join('; ')
  const cspMeta = `<meta http-equiv="Content-Security-Policy" content="${policy}">`
  // Fit the reviewed scene, its caption and its pause control into a compact preview.
  const previewStyle = `<style>
    body { min-height: 0; height: 100dvh; padding: 0; }
    main.card { display: flex; flex-direction: column; width: 100%; height: 100%; border: 0; border-radius: 0; }
    main.card header, main.card footer { flex: none; gap: 6px; padding: 4px 8px; }
    main.card .eyebrow { font-size: 8px; margin: 0; }
    main.card h1 { font-size: 14px; }
    main.card > svg { flex: 1 1 0; min-height: 0; }
    main.card footer p, main.card button { font-size: 10px; line-height: 1.3; }
    main.card button { padding: 4px 8px; }
  </style>`
  const documentHtml =
    prologue[0] +
    cspMeta +
    html
      .slice(prologue[0].length)
      .replace(/<\/head>/i, previewStyle + '</head>')
  return { ...record, documentHtml }
}
