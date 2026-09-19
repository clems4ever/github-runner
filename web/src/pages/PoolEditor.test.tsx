import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MantineProvider } from '@mantine/core'
import { describe, expect, it, vi } from 'vitest'
import { PoolEditor } from './PoolEditor'
import { api, ApiError, emptyPool } from '../api'

function renderEditor(pool = emptyPool(1)) {
  return render(
    <MantineProvider>
      <PoolEditor
        pool={pool}
        credentials={[{ id: 1, name: 'pat', kind: 'pat', hint: '…1234', createdAt: '' }]}
        onSaved={vi.fn()}
        onCancel={vi.fn()}
      />
    </MantineProvider>,
  )
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('PoolEditor', () => {
  it('shows the labels the runners will register with, before saving', async () => {
    renderEditor()
    expect(screen.getByText('self-hosted')).toBeInTheDocument()
    expect(screen.getByText('vm')).toBeInTheDocument()

    await userEvent.click(screen.getByRole('switch', { name: /Nested virtualisation/ }))
    expect(screen.getByText('nestedvirt')).toBeInTheDocument()
  })

  it('warns that a container with nested virtualisation is a weaker boundary', async () => {
    renderEditor()
    expect(screen.queryByText(/hole in an already weaker boundary/)).not.toBeInTheDocument()

    await userEvent.click(screen.getByText('Container'))
    await userEvent.click(screen.getByRole('switch', { name: /Nested virtualisation/ }))

    expect(screen.getByText(/hole in an already weaker boundary/)).toBeInTheDocument()
  })

  it('offers a docker daemon to containers, which have none of their own', async () => {
    renderEditor()
    // A machine boots with one installed in its image, so there is nothing to
    // offer and nothing the daemon would accept.
    expect(screen.queryByRole('switch', { name: /Docker in Docker/ })).not.toBeInTheDocument()

    await userEvent.click(screen.getByText('Container'))
    await userEvent.click(screen.getByRole('switch', { name: /Docker in Docker/ }))

    expect(screen.getByText('dind')).toBeInTheDocument()
    expect(screen.getByText(/privileged container/)).toBeInTheDocument()
  })

  it('drops the docker daemon when the pool becomes a machine', async () => {
    // Otherwise the pool is refused on save, for a switch that is no longer on
    // screen to explain it.
    renderEditor()
    await userEvent.click(screen.getByText('Container'))
    await userEvent.click(screen.getByRole('switch', { name: /Docker in Docker/ }))
    expect(screen.getByText('dind')).toBeInTheDocument()

    await userEvent.click(screen.getByText('Virtual machine'))
    expect(screen.queryByText('dind')).not.toBeInTheDocument()

    await userEvent.click(screen.getByText('Container'))
    expect(screen.getByRole('switch', { name: /Docker in Docker/ })).not.toBeChecked()
  })

  it('offers what to bake in on either runtime', async () => {
    // Both were machines only, which left an ephemeral container pool
    // installing the same toolchain on every job for ever. The fields mean the
    // same thing on either runtime now; only how the image is built differs.
    renderEditor()
    expect(screen.getByText(/apt packages baked in/)).toBeInTheDocument()
    expect(screen.getByText(/while the image is built/)).toBeInTheDocument()

    await userEvent.click(screen.getByText('Container'))
    expect(screen.getByText(/apt packages baked in/)).toBeInTheDocument()
    expect(screen.getByText(/in a Dockerfile step on top of the image above/)).toBeInTheDocument()
  })

  it('hides the disk size for containers, which have none', async () => {
    renderEditor()
    expect(screen.getByRole('textbox', { name: 'Disk (GiB)' })).toBeInTheDocument()
    await userEvent.click(screen.getByText('Container'))
    expect(screen.queryByRole('textbox', { name: 'Disk (GiB)' })).not.toBeInTheDocument()
  })

  it('refuses a repository that is not owner/name', async () => {
    renderEditor()
    await userEvent.type(screen.getByRole('textbox', { name: /^Name/ }), 'web')
    await userEvent.type(screen.getByPlaceholderText('owner/repository'), 'runyard')
    await userEvent.click(screen.getByRole('button', { name: 'Create' }))
    expect(await screen.findByText('A repository is owner/name')).toBeInTheDocument()
  })

  it('says what the pool will cost in memory at its maximum', async () => {
    renderEditor({ ...emptyPool(1), minReplicas: 1, maxReplicas: 3, memoryMb: 4096 })
    expect(screen.getByText(/12 GiB of memory at once/)).toBeInTheDocument()
  })
})

describe('when GitHub refuses the pool', () => {
  // An app cannot widen its own access — GitHub does not allow it — so the most
  // the daemon can do is say what is wrong and put the page that fixes it one
  // click away.
  it('shows the reason on the form, with a way to fix it', async () => {
    vi.spyOn(api, 'createPool').mockRejectedValue(
      new ApiError(
        'GitHub returned 404 for clems4ever/claude-control — the app is authenticated but has no installation covering it',
        400,
        'https://github.com/settings/installations/42',
      ),
    )

    renderEditor()
    await userEvent.type(screen.getByRole('textbox', { name: /^Name/ }), 'web')
    await userEvent.type(screen.getByPlaceholderText('owner/repository'), 'clems4ever/claude-control')
    await userEvent.click(screen.getByRole('button', { name: 'Create' }))

    expect(await screen.findByText(/no installation covering it/)).toBeInTheDocument()
    const grant = screen.getByRole('link', { name: /Grant access on GitHub/ })
    expect(grant).toHaveAttribute('href', 'https://github.com/settings/installations/42')
  })

  // A token has no installation page, so there is nothing to link to and the
  // message stands alone.
  it('offers no link when there is nowhere to send anyone', async () => {
    vi.spyOn(api, 'createPool').mockRejectedValue(new ApiError('GitHub returned 403', 400))

    renderEditor()
    await userEvent.type(screen.getByRole('textbox', { name: /^Name/ }), 'web')
    await userEvent.type(screen.getByPlaceholderText('owner/repository'), 'o/r')
    await userEvent.click(screen.getByRole('button', { name: 'Create' }))

    expect(await screen.findByText(/GitHub returned 403/)).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /Grant access/ })).not.toBeInTheDocument()
  })
})
