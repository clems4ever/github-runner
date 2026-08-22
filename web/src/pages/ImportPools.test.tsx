import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MantineProvider } from '@mantine/core'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ImportPools } from './ImportPools'
import { pretendNarrow } from '../test-setup'
import { api, ApiError, type ImportReport, type Pool } from '../api'

const CREDENTIALS = [
  { id: 3, name: 'fleet app', kind: 'app' as const, appId: 42, hint: 'app 42', createdAt: '' },
]

const TEMPLATE = JSON.stringify({
  version: 1,
  name: 'github-runner CI',
  description: 'Containers for the toolchain jobs, machines for the rest',
  pools: [
    { name: 'ci-container', scope: 'clems4ever/github-runner', runtime: 'container' },
    { name: 'ci-vm', scope: 'clems4ever/github-runner', runtime: 'vm' },
  ],
})

function pool(over: Partial<Pool>): Pool {
  return {
    id: 1,
    name: 'ci-container',
    scopeKind: 'repository',
    scope: 'clems4ever/github-runner',
    runtime: 'container',
    nested: false,
    ephemeral: true,
    minReplicas: 1,
    maxReplicas: 3,
    labels: [],
    cpus: 2,
    memoryMb: 4096,
    diskGb: 0,
    image: 'default',
    credentialId: 3,
    enabled: true,
    createdAt: '',
    updatedAt: '',
    ...over,
  }
}

const PREVIEW: ImportReport = {
  dryRun: true,
  name: 'github-runner CI',
  description: 'Containers for the toolchain jobs, machines for the rest',
  pools: [
    { name: 'ci-container', action: 'create', pool: pool({}) },
    {
      name: 'ci-vm',
      action: 'create',
      pool: pool({ id: 2, name: 'ci-vm', runtime: 'vm', cpus: 4, memoryMb: 8192, diskGb: 40 }),
    },
  ],
}

function renderImport(onImported = vi.fn()) {
  return render(
    <MantineProvider>
      <ImportPools credentials={CREDENTIALS} onImported={onImported} onCancel={vi.fn()} />
    </MantineProvider>,
  )
}

async function paste(text: string) {
  const box = screen.getByRole('textbox', { name: /Template/ })
  // Pasting rather than typing: the text is JSON, and typing it a character at
  // a time is both slow and a different thing from what a person does.
  await userEvent.click(box)
  await userEvent.paste(text)
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('ImportPools', () => {
  it('will not preview an empty box', () => {
    renderImport()
    expect(screen.getByRole('button', { name: 'Preview' })).toBeDisabled()
  })

  // The preview is a real import that was rolled back, so it is asked for
  // before anything is written, and the button underneath says what it will do.
  it('previews before importing, and imports what was previewed', async () => {
    const importPools = vi.spyOn(api, 'importPools').mockResolvedValue(PREVIEW)
    const onImported = vi.fn()
    renderImport(onImported)

    await paste(TEMPLATE)
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }))

    await screen.findByText('ci-container')
    expect(screen.getByText('ci-vm')).toBeInTheDocument()
    expect(importPools.mock.calls[0][0]).toMatchObject({ credentialId: 3, dryRun: true })
    // Nothing has been written yet, whatever the table says.
    expect(importPools).toHaveBeenCalledTimes(1)

    importPools.mockResolvedValue({ ...PREVIEW, dryRun: false })
    await userEvent.click(screen.getByRole('button', { name: 'Import 2 pools' }))

    await waitFor(() => expect(onImported).toHaveBeenCalled())
    expect(importPools.mock.calls[1][0]).toMatchObject({ dryRun: false })
  })

  it('sends the document as JSON, not as a string', async () => {
    const importPools = vi.spyOn(api, 'importPools').mockResolvedValue(PREVIEW)
    renderImport()

    await paste(TEMPLATE)
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }))

    await waitFor(() => expect(importPools).toHaveBeenCalled())
    const sent = importPools.mock.calls[0][0].document as { version: number; pools: unknown[] }
    expect(sent.version).toBe(1)
    expect(sent.pools).toHaveLength(2)
  })

  it('says so when the text is not JSON at all, without asking the daemon', async () => {
    const importPools = vi.spyOn(api, 'importPools')
    renderImport()

    await paste('{ this is not json')
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }))

    expect(await screen.findByText(/not valid JSON/)).toBeInTheDocument()
    expect(importPools).not.toHaveBeenCalled()
  })

  // Everything about what a document means is the daemon's answer, so its
  // refusal is what is shown — in full, on the form.
  it('shows what the daemon said was wrong with the template', async () => {
    vi.spyOn(api, 'importPools').mockRejectedValue(
      new ApiError('pool "greedy": cpus 9000: want 1 to 64', 400),
    )
    renderImport()

    await paste(TEMPLATE)
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }))

    expect(await screen.findByText(/cpus 9000/)).toBeInTheDocument()
    // And no table, so there is nothing to press Import under.
    expect(screen.queryByRole('button', { name: /^Import/ })).not.toBeInTheDocument()
  })

  it('offers the page that fixes a refusal from GitHub', async () => {
    vi.spyOn(api, 'importPools').mockRejectedValue(
      new ApiError('GitHub returned 404 for clems4ever/github-runner', 400, 'https://github.com/settings/installations/42'),
    )
    renderImport()

    await paste(TEMPLATE)
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }))

    const grant = await screen.findByRole('link', { name: /Grant access on GitHub/ })
    expect(grant).toHaveAttribute('href', 'https://github.com/settings/installations/42')
  })

  it('sends the scope override only when one was typed', async () => {
    const importPools = vi.spyOn(api, 'importPools').mockResolvedValue(PREVIEW)
    renderImport()

    await paste(TEMPLATE)
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }))
    await waitFor(() => expect(importPools).toHaveBeenCalled())
    expect(importPools.mock.calls[0][0].scope).toBeUndefined()

    await userEvent.type(screen.getByPlaceholderText('owner/repository'), 'someone/else')
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }))

    await waitFor(() => expect(importPools).toHaveBeenCalledTimes(2))
    expect(importPools.mock.calls[1][0]).toMatchObject({
      scope: 'someone/else',
      scopeKind: 'repository',
    })
  })

  it('asks before writing over pools that already exist', async () => {
    const importPools = vi.spyOn(api, 'importPools').mockResolvedValue(PREVIEW)
    renderImport()

    await paste(TEMPLATE)
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }))
    await waitFor(() => expect(importPools).toHaveBeenCalled())
    expect(importPools.mock.calls[0][0].replaceExisting).toBe(false)

    await userEvent.click(screen.getByRole('checkbox', { name: /Import over pools/ }))
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }))

    await waitFor(() => expect(importPools).toHaveBeenCalledTimes(2))
    expect(importPools.mock.calls[1][0].replaceExisting).toBe(true)
  })

  // A preview belongs to the text it was taken from. Editing the template after
  // previewing must not leave an Import button that imports something else.
  it('drops the preview when the template is edited', async () => {
    vi.spyOn(api, 'importPools').mockResolvedValue(PREVIEW)
    renderImport()

    await paste(TEMPLATE)
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }))
    expect(await screen.findByRole('button', { name: 'Import 2 pools' })).toBeInTheDocument()

    await paste(' ')
    expect(screen.queryByRole('button', { name: /^Import/ })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Preview' })).toBeInTheDocument()
  })

  it('marks a pool that would be written over', async () => {
    vi.spyOn(api, 'importPools').mockResolvedValue({
      ...PREVIEW,
      pools: [{ name: 'ci-container', action: 'update', pool: pool({}) }],
    })
    renderImport()

    await paste(TEMPLATE)
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }))

    expect(await screen.findByText(/replaces the pool of this name/)).toBeInTheDocument()
    expect(screen.getByText(/replaced gracefully, as it finishes the job/)).toBeInTheDocument()
  })

  // The preview is the whole point of the screen: it is read before the button
  // under it is pressed. A six-column table on a phone is not read.
  it('previews as a card per pool on a phone', async () => {
    const restore = pretendNarrow()
    try {
      vi.spyOn(api, 'importPools').mockResolvedValue(PREVIEW)
      renderImport()

      await paste(TEMPLATE)
      await userEvent.click(screen.getByRole('button', { name: 'Preview' }))

      await screen.findByText('ci-container')
      expect(screen.queryByRole('table')).not.toBeInTheDocument()
      expect(screen.getByText('ci-vm')).toBeInTheDocument()
      expect(screen.getAllByText('new')).toHaveLength(2)
      expect(screen.getByText('4 vCPU · 8 GiB · 40 GiB')).toBeInTheDocument()
    } finally {
      restore()
    }
  })
})
