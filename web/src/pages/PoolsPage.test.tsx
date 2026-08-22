import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MantineProvider } from '@mantine/core'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { PoolsPage } from './PoolsPage'
import { pretendNarrow } from '../test-setup'
import type { Credential, Pool, Runner, Scale } from '../api'

const CREDENTIALS: Credential[] = [
  { id: 1, name: 'fleet app', kind: 'app', appId: 42, hint: 'app 42', createdAt: '' },
]

const pool = (over: Partial<Pool> = {}): Pool => ({
  id: 1,
  name: 'web',
  scopeKind: 'repository',
  scope: 'clems4ever/github-runner',
  runtime: 'vm',
  nested: false,
  ephemeral: true,
  minReplicas: 1,
  maxReplicas: 4,
  labels: ['gpu'],
  cpus: 2,
  memoryMb: 4096,
  diskGb: 40,
  image: 'default',
  credentialId: 1,
  enabled: true,
  createdAt: '',
  updatedAt: '',
  ...over,
})

function renderPage(
  pools: Pool[],
  runners: Runner[] = [],
  scaling: Record<string, Scale> = {},
) {
  return render(
    <MantineProvider>
      <PoolsPage
        pools={pools}
        credentials={CREDENTIALS}
        runners={runners}
        scaling={scaling}
        onChange={vi.fn()}
      />
    </MantineProvider>,
  )
}

describe('PoolsPage', () => {
  it('lists the pools in a table on a wide screen', () => {
    renderPage([pool()])
    expect(screen.getByRole('table')).toBeInTheDocument()
    expect(screen.getByText('web')).toBeInTheDocument()
  })

  it('says what to do when there are no pools yet', () => {
    renderPage([])
    expect(screen.getByText('No pools yet')).toBeInTheDocument()
  })

  describe('on a phone', () => {
    let restore = () => {}
    afterEach(() => restore())

    it('draws a card per pool instead of a table', () => {
      restore = pretendNarrow()
      renderPage([pool()])

      expect(screen.queryByRole('table')).not.toBeInTheDocument()
      expect(screen.getByText('web')).toBeInTheDocument()
      expect(screen.getByText('clems4ever/github-runner')).toBeInTheDocument()
      expect(screen.getByText('2 vCPU · 4 GiB · 40 GiB')).toBeInTheDocument()
    })

    // The bug this page had: the menu that edits and deletes a pool lives in the
    // eighth column, which on a phone is past the right-hand edge of a table
    // that does not scroll. There was no way to edit a pool from a phone at all.
    it('keeps the actions menu within reach', async () => {
      restore = pretendNarrow()
      renderPage([pool()])

      await userEvent.click(screen.getByRole('button', { name: 'Actions for web' }))

      expect(await screen.findByText('Edit')).toBeInTheDocument()
      expect(screen.getByText('Delete')).toBeInTheDocument()
    })

    it('keeps the enabled switch, which is the other thing a row can do', () => {
      restore = pretendNarrow()
      renderPage([pool()])
      expect(screen.getByRole('switch', { name: 'Enable web' })).toBeChecked()
    })
  })
})
