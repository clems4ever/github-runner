import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MantineProvider } from '@mantine/core'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { CredentialForm } from './CredentialForm'
import { api, type Credential } from '../api'

vi.mock('../api', async () => {
  const actual = await vi.importActual<typeof import('../api')>('../api')
  return {
    ...actual,
    api: { ...actual.api, createCredential: vi.fn(), rotateCredential: vi.fn() },
  }
})

const create = vi.mocked(api.createCredential)
const rotate = vi.mocked(api.rotateCredential)

const KEY = '-----BEGIN RSA PRIVATE KEY-----\nMIIEpQIB\n-----END RSA PRIVATE KEY-----'

function renderForm(rotating: Credential | null = null) {
  return render(
    <MantineProvider>
      <CredentialForm rotating={rotating} onSaved={vi.fn()} onCancel={vi.fn()} />
    </MantineProvider>,
  )
}

describe('CredentialForm', () => {
  beforeEach(() => {
    create.mockReset().mockResolvedValue({} as Credential)
    rotate.mockReset().mockResolvedValue(undefined)
  })

  // An app is the better credential of the two, so it is what the form offers
  // first rather than something to go looking for.
  it('offers a GitHub App by default', () => {
    renderForm()
    expect(screen.getByLabelText(/App ID/)).toBeInTheDocument()
    expect(screen.queryByLabelText(/^Token/)).not.toBeInTheDocument()
  })

  it('sends an app with its id and key', async () => {
    renderForm()
    await userEvent.type(screen.getByLabelText(/^Name/), 'runyard app')
    await userEvent.type(screen.getByLabelText(/App ID/), '123456')
    await userEvent.type(screen.getByLabelText(/Private key/), KEY)
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() =>
      expect(create).toHaveBeenCalledWith(
        expect.objectContaining({ kind: 'app', appId: 123456, secret: expect.stringContaining('PRIVATE KEY') }),
      ),
    )
  })

  // Zero is not an installation, it is "find it", so it must not be sent as one.
  it('leaves an unspecified installation out', async () => {
    renderForm()
    await userEvent.type(screen.getByLabelText(/^Name/), 'app')
    await userEvent.type(screen.getByLabelText(/App ID/), '1')
    await userEvent.type(screen.getByLabelText(/Private key/), KEY)
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(create).toHaveBeenCalled())
    expect(create.mock.calls[0][0].installationId).toBeUndefined()
  })

  it('switches to a token, and asks for nothing an app needs', async () => {
    renderForm()
    await userEvent.click(screen.getByText('Personal access token'))

    expect(screen.queryByLabelText(/App ID/)).not.toBeInTheDocument()
    expect(screen.queryByLabelText(/Private key/)).not.toBeInTheDocument()

    await userEvent.type(screen.getByLabelText(/^Name/), 'pat')
    await userEvent.type(screen.getByLabelText(/^Token/), 'github_pat_11ABC')
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() =>
      expect(create).toHaveBeenCalledWith(
        expect.objectContaining({ kind: 'pat', secret: 'github_pat_11ABC' }),
      ),
    )
    expect(create.mock.calls[0][0].appId).toBeUndefined()
  })

  it('will not save until it has what it needs', async () => {
    renderForm()
    const save = screen.getByRole('button', { name: 'Save' })
    expect(save).toBeDisabled()

    await userEvent.type(screen.getByLabelText(/^Name/), 'app')
    await userEvent.type(screen.getByLabelText(/Private key/), KEY)
    // Still no app id.
    expect(save).toBeDisabled()

    await userEvent.type(screen.getByLabelText(/App ID/), '1')
    expect(save).toBeEnabled()
  })

  // Replacing a secret settles the kind and the app already: only the secret
  // itself is in question.
  it('asks only for the new secret when replacing one', async () => {
    renderForm({ id: 1, name: 'app', kind: 'app', appId: 42, hint: 'app 42', createdAt: '' })

    expect(screen.queryByLabelText(/^Name/)).not.toBeInTheDocument()
    expect(screen.queryByLabelText(/App ID/)).not.toBeInTheDocument()

    await userEvent.type(screen.getByLabelText(/Private key/), KEY)
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(rotate).toHaveBeenCalledWith(1, expect.stringContaining('PRIVATE KEY')))
  })

  it('asks for a token when replacing a token', () => {
    renderForm({ id: 2, name: 'pat', kind: 'pat', hint: '…1234', createdAt: '' })
    expect(screen.getByLabelText(/^Token/)).toBeInTheDocument()
    expect(screen.queryByLabelText(/Private key/)).not.toBeInTheDocument()
  })

  // The part people get wrong: the app has to be installed, and it does not
  // have to be public or reachable.
  it('says how to set the app up', () => {
    renderForm()
    expect(screen.getByText(/Only on this account/)).toBeInTheDocument()
    expect(screen.getByText(/Administration: Read and write/)).toBeInTheDocument()
    expect(screen.getByText(/nothing here needs to be reachable from the internet/i)).toBeInTheDocument()
  })
})
