import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MantineProvider } from '@mantine/core'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { CredentialsPage } from './CredentialsPage'
import { pretendNarrow } from '../test-setup'
import type { Credential, Pool } from '../api'

const CREDENTIALS: Credential[] = [
  { id: 1, name: 'fleet app', kind: 'app', appId: 42, hint: '…f4c1', createdAt: '' },
  { id: 2, name: 'a token', kind: 'pat', hint: '…9a2b', createdAt: '' },
]

const POOLS = [{ id: 1, name: 'web', credentialId: 1 } as Pool]

function renderPage(credentials = CREDENTIALS, pools = POOLS) {
  return render(
    <MantineProvider>
      <CredentialsPage credentials={credentials} pools={pools} onChange={vi.fn()} />
    </MantineProvider>,
  )
}

describe('CredentialsPage', () => {
  it('lists the credentials in a table on a wide screen', () => {
    renderPage()
    expect(screen.getByRole('table')).toBeInTheDocument()
    expect(screen.getByText('fleet app')).toBeInTheDocument()
  })

  it('says what to do when there are none yet', () => {
    renderPage([], [])
    expect(screen.getByText('No credentials yet')).toBeInTheDocument()
  })

  describe('on a phone', () => {
    let restore = () => {}
    afterEach(() => restore())

    it('draws a card per credential instead of a table', () => {
      restore = pretendNarrow()
      renderPage()

      expect(screen.queryByRole('table')).not.toBeInTheDocument()
      expect(screen.getByText('fleet app')).toBeInTheDocument()
      expect(screen.getByText('…f4c1')).toBeInTheDocument()
      // Which pools depend on it is what decides whether it can be deleted, so
      // it is not a column to drop on a narrow screen.
      expect(screen.getByText('web')).toBeInTheDocument()
    })

    it('keeps the actions menu within reach', async () => {
      restore = pretendNarrow()
      renderPage()

      await userEvent.click(screen.getByRole('button', { name: 'Actions for fleet app' }))

      expect(await screen.findByText('Replace private key')).toBeInTheDocument()
      // Still in use by the web pool, so deleting it is refused here rather
      // than by the daemon after the fact.
      expect(screen.getByText('Delete').closest('button')).toHaveAttribute('data-disabled')
    })
  })
})
