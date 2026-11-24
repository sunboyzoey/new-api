import { createHash, webcrypto } from 'node:crypto'

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { PelicanResultCard, PelicanResultGrid } from '../group-history'
import { HtmlTrial } from '../html-trial'
import { getHtmlTrial } from '../html-trial-document'

const review = vi.hoisted(() => ({
  target_id: 'fixture-target',
  group: 'fixture-group',
  model: 'fixture-model',
  finished_at: 1700000000000,
  documentSha256:
    '279381fcdad27dcedfb9f50a347296184e5646011ca063dae2f0c2a95053972b',
  scriptSha256: 'uwLTrg6EQuALZ1AkHt+CShhihLXFsVPB7y8VVm/lWqQ=',
}))
vi.mock('../html-trial-document', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../html-trial-document')>()),
  reviewedHtmlTrial: review,
}))

const script = 'document.getElementById("toggle").title = "Pause"'
const html = `<!DOCTYPE html>\n<html lang="en"><head><style>svg{color:green}</style></head><body><button id="toggle" type="button">Pause</button><svg xmlns="http://www.w3.org/2000/svg"><g id="wheel"></g><use href="#wheel"/></svg><script>${
  script
}</script></body></html>`
const record = {
  schema_version: 1,
  target_id: review.target_id,
  group: review.group,
  model: review.model,
  finished_at: review.finished_at,
  html,
}
const group = {
  id: review.target_id,
  group: review.group,
  models: [review.model],
}
const fetchManifest = vi.fn<typeof fetch>()

beforeEach(() => {
  vi.stubGlobal('crypto', webcrypto)
  vi.stubGlobal('fetch', fetchManifest)
})
afterEach(() => {
  fetchManifest.mockReset()
  vi.unstubAllGlobals()
})

it('displays the reviewed trial only in its group, including after changing groups with a warm cache', async () => {
  fetchManifest.mockResolvedValueOnce(Response.json(record))
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  })
  function view(selected: typeof group) {
    return (
      <QueryClientProvider client={client}>
        <HtmlTrial group={selected} />
      </QueryClientProvider>
    )
  }
  const { rerender, container } = render(view({ ...group, id: 'other-target' }))
  rerender(view({ ...group, group: 'renamed' }))
  rerender(view({ ...group, models: ['other-model'] }))
  expect(fetchManifest).not.toHaveBeenCalled()
  expect(
    screen.queryByTitle('One-time HTML drawing test')
  ).not.toBeInTheDocument()

  rerender(view(group))
  const frame = await screen.findByTitle('One-time HTML drawing test')
  expect(screen.getByText('fixture-model')).toBeInTheDocument()
  expect(container.querySelector('time')).toHaveAttribute(
    'datetime',
    new Date(record.finished_at).toISOString()
  )
  expect(fetchManifest).toHaveBeenCalledTimes(1)
  expect(fetchManifest).toHaveBeenCalledWith(
    `/static/monitor/pelican-html-trial.json?v=${review.documentSha256}`,
    expect.objectContaining({
      signal: expect.anything(),
      credentials: 'omit',
      mode: 'same-origin',
      redirect: 'error',
      referrerPolicy: 'no-referrer',
    })
  )
  expect(frame).toHaveAttribute('sandbox', 'allow-scripts')
  expect(frame).toHaveAttribute('referrerpolicy', 'no-referrer')
  expect(frame).not.toHaveAttribute('tabindex', '-1')
  expect(
    screen.queryByRole('button', { name: 'Pause' })
  ).not.toBeInTheDocument()

  const src = frame.getAttribute('srcdoc') ?? ''
  const framedDocument = new DOMParser().parseFromString(src, 'text/html')
  expect(framedDocument.doctype?.name).toBe('html')
  expect(framedDocument.documentElement.lang).toBe('en')
  const policy = framedDocument.head.firstElementChild
  expect(policy?.getAttribute('http-equiv')).toBe('Content-Security-Policy')
  const directives = policy?.getAttribute('content')?.split('; ')
  expect(directives).toEqual(
    expect.arrayContaining([
      "default-src 'none'",
      `script-src 'sha256-${review.scriptSha256}'`,
      "script-src-attr 'none'",
      "form-action 'none'",
      "frame-src 'none'",
      "base-uri 'none'",
      "connect-src 'none'",
      "object-src 'none'",
    ])
  )
  expect(framedDocument.head.querySelector('style')?.textContent).toBe(
    'svg{color:green}'
  )
  expect(framedDocument.body.querySelector('button')?.textContent).toBe('Pause')
  expect(framedDocument.body.querySelector('script')?.textContent).toBe(script)

  rerender(view({ ...group, group: 'renamed' }))
  expect(
    screen.queryByTitle('One-time HTML drawing test')
  ).not.toBeInTheDocument()
  rerender(view(group))
  expect(
    await screen.findByTitle('One-time HTML drawing test')
  ).toBeInTheDocument()
  expect(fetchManifest).toHaveBeenCalledTimes(1)
})

it.each([
  ['target', { target_id: 'other-target' }],
  ['group name', { group: 'other-group' }],
  ['model', { model: 'other-model' }],
  ['completion time', { finished_at: record.finished_at + 1 }],
  ['unreviewed content', { html: html.replace('color:green', 'color:red') }],
  [
    'script permission from the manifest',
    { scriptSha256: review.scriptSha256 },
  ],
] as const)('omits a trial with changed %s', async (_, change) => {
  fetchManifest.mockResolvedValueOnce(Response.json({ ...record, ...change }))
  expect(await getHtmlTrial(review, new AbortController().signal)).toBeNull()
})

it('gives the one-time trial and scheduled drawings the same compact preview in the results grid', async () => {
  fetchManifest.mockResolvedValueOnce(Response.json(record))
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  })
  const svg = '<svg xmlns="http://www.w3.org/2000/svg"><circle r="5"/></svg>'
  const result = {
    id: 2,
    target_id: group.id,
    group: group.group,
    model: 'scheduled-model',
    started_at: record.finished_at,
    finished_at: record.finished_at,
    duration_ms: 100,
    status: 'pending_review' as const,
    error: '',
  }
  const scheduledHtml = `<!DOCTYPE html><html><head></head><body><img src="data:image/svg+xml;base64,${btoa(svg)}"></body></html>`

  render(
    <QueryClientProvider client={client}>
      <PelicanResultGrid>
        <HtmlTrial group={group} />
        <PelicanResultCard result={{ ...result, html: scheduledHtml }} />
        <PelicanResultCard result={{ ...result, svg }} />
      </PelicanResultGrid>
    </QueryClientProvider>
  )

  const trial = await screen.findByTitle('One-time HTML drawing test')
  const scheduled = screen.getByTitle('Pelican bicycle HTML animation')
  const historical = screen.getByRole('img', {
    name: 'Pelican bicycle SVG drawing',
  })
  for (const preview of [trial, scheduled, historical]) {
    expect(preview).toHaveClass('aspect-4/3', 'w-full')
    expect(preview.closest('[data-slot="card"]')).toHaveAttribute(
      'data-size',
      'sm'
    )
  }
  expect(scheduled).toHaveAttribute('sandbox', '')
  expect(scheduled).toHaveAttribute('referrerpolicy', 'no-referrer')
  const doc = new DOMParser().parseFromString(
    scheduled.getAttribute('srcdoc') ?? '',
    'text/html'
  )
  const policy = doc.head.firstElementChild
  expect(policy?.getAttribute('http-equiv')).toBe('Content-Security-Policy')
  expect(policy?.getAttribute('content')).toContain("default-src 'none'")
  expect(policy?.getAttribute('content')).toContain('img-src data:')
  expect(doc.body.querySelector('img')?.getAttribute('src')).toBe(
    `data:image/svg+xml;base64,${btoa(svg)}`
  )
  expect(historical).toHaveAttribute(
    'src',
    `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`
  )
})

it.each([
  ['missing DOCTYPE', html.replace('<!DOCTYPE html>', '')],
  [
    'document with a navigation link',
    html.replace('</body>', '<a href="#wheel">wheel</a></body>'),
  ],
  [
    'additional embedded frame',
    html.replace('</body>', '<iframe></iframe></body>'),
  ],
  ['form', html.replace('</body>', '<form></form></body>')],
  ['different inline script', html.replace(script, 'document.title = "Wheel"')],
] as const)('omits a reviewed fixture with %s', async (_, documentHtml) => {
  fetchManifest.mockResolvedValueOnce(
    Response.json({ ...record, html: documentHtml })
  )
  const policy = {
    ...review,
    documentSha256: createHash('sha256').update(documentHtml).digest('hex'),
  }
  expect(await getHtmlTrial(policy, new AbortController().signal)).toBeNull()
})

it('leaves the trial absent if the manifest is missing or the static server returns an HTML fallback', async () => {
  const signal = new AbortController().signal
  fetchManifest.mockResolvedValueOnce(new Response(null, { status: 404 }))
  expect(await getHtmlTrial(review, signal)).toBeNull()
  fetchManifest.mockResolvedValueOnce(
    new Response(html, { headers: { 'content-type': 'text/html' } })
  )
  expect(await getHtmlTrial(review, signal)).toBeNull()
})

it('leaves the trial absent when the browser cannot verify its digest', async () => {
  vi.stubGlobal('crypto', {})
  expect(await getHtmlTrial(review, new AbortController().signal)).toBeNull()
  expect(fetchManifest).not.toHaveBeenCalled()
})
