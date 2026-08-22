import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MantineProvider } from '@mantine/core'
import { describe, expect, it, vi } from 'vitest'
import { PoolEditor } from './PoolEditor'
import { emptyPool } from '../api'

function renderEditor(pool = emptyPool(1)) {
  return render(
    <MantineProvider>
      <PoolEditor
        pool={pool}
        credentials={[{ id: 1, name: 'pat', hint: '…1234', createdAt: '' }]}
        onSaved={vi.fn()}
        onCancel={vi.fn()}
      />
    </MantineProvider>,
  )
}

describe('PoolEditor', () => {
  it('shows the labels the runners will register with, before saving', async () => {
    renderEditor()
    expect(screen.getByText('self-hosted')).toBeInTheDocument()
    expect(screen.getByText('vm')).toBeInTheDocument()

    await userEvent.click(screen.getByRole('switch', { name: /Nested virtualisation/ }))
    expect(screen.getByText('nested')).toBeInTheDocument()
  })

  it('warns that a container with nested virtualisation is a weaker boundary', async () => {
    renderEditor()
    expect(screen.queryByText(/hole in an already weaker boundary/)).not.toBeInTheDocument()

    await userEvent.click(screen.getByText('Container'))
    await userEvent.click(screen.getByRole('switch', { name: /Nested virtualisation/ }))

    expect(screen.getByText(/hole in an already weaker boundary/)).toBeInTheDocument()
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
