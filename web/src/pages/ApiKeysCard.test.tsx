import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MantineProvider } from '@mantine/core'
import { Notifications, notifications } from '@mantine/notifications'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiKeysCard } from './ApiKeysCard'
import { api, type ApiKey } from '../api'

vi.mock('../api', async () => {
  const actual = await vi.importActual<typeof import('../api')>('../api')
  return {
    ...actual,
    api: { ...actual.api, apiKeys: vi.fn(), createApiKey: vi.fn(), deleteApiKey: vi.fn() },
  }
})

const monitoring: ApiKey = {
  id: 1,
  name: 'monitoring',
  scope: 'read',
  hint: 'A1b2C3',
  createdAt: '2026-09-01T10:00:00Z',
}

const terraform: ApiKey = {
  id: 2,
  name: 'terraform',
  scope: 'admin',
  hint: 'Z9y8X7',
  createdAt: '2026-09-01T10:00:00Z',
  expiresAt: '2026-12-01T10:00:00Z',
  lastUsedAt: '2026-09-18T09:00:00Z',
}

function stored(keys: ApiKey[] = []) {
  vi.mocked(api.apiKeys).mockResolvedValue(keys)
}

async function renderCard() {
  render(
    <MantineProvider>
      <Notifications />
      <ApiKeysCard />
    </MantineProvider>,
  )
  // The keys are fetched on mount, and nothing about the card can be asserted
  // until they have arrived.
  await screen.findByText('API keys')
}

beforeEach(() => {
  notifications.clean()
  // Calls are not cleared between tests by default, and several of the
  // assertions below read the first call to a mock: without this they would be
  // reading what the previous test did.
  vi.clearAllMocks()
  stored()
  vi.mocked(api.deleteApiKey).mockResolvedValue(undefined)
})

describe('the api keys card', () => {
  it('says what a key is for when there are none', async () => {
    await renderCard()
    expect(screen.getByText(/Everything calling this daemon is doing so as you/)).toBeInTheDocument()
  })

  // The constraint the design rests on, said where a key is handed out rather
  // than only in the documentation.
  it('says that a key cannot reach the other keys or the password', async () => {
    await renderCard()
    expect(screen.getByText(/No key can read or create another key/)).toBeInTheDocument()
  })

  it('lists what an operator has to go on', async () => {
    stored([monitoring, terraform])
    await renderCard()

    const row = screen.getByText('terraform').closest('tr')
    expect(row).not.toBeNull()
    expect(within(row!).getByText('admin')).toBeInTheDocument()
    expect(within(row!).getByText('rf_Z9y8X7…')).toBeInTheDocument()
  })

  // A blank cell reads as a value that failed to load, so the absence of an
  // expiry and of any use are both spelled out.
  it('says "never" for a key with no expiry that has never been used', async () => {
    stored([monitoring])
    await renderCard()

    const row = screen.getByText('monitoring').closest('tr')
    expect(within(row!).getAllByText('never')).toHaveLength(2)
  })

  it('shows only the front of a key, never a whole one', async () => {
    stored([monitoring, terraform])
    await renderCard()

    // The hint is six characters; anything key-length on this page would be the
    // key itself.
    expect(screen.getByText('rf_A1b2C3…')).toBeInTheDocument()
    expect(screen.queryByText(/rf_[A-Za-z0-9_-]{20,}/)).not.toBeInTheDocument()
  })
})

describe('creating a key', () => {
  const minted = {
    apiKey: { ...monitoring, id: 3, name: 'ci', scope: 'admin' as const },
    secret: 'rf_Bxv3Qr9LmT0aZs6Yk2Wd8Np1Ue5Gc7Hj4Rf0Ao3Ib',
  }

  async function create(name = 'ci') {
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'New key' }))
    await user.type(await screen.findByLabelText('Name'), name)
    await user.click(screen.getByRole('button', { name: 'Create key' }))
    return user
  }

  it('shows the key once, with the warning that it cannot be shown again', async () => {
    vi.mocked(api.createApiKey).mockResolvedValue(minted)
    await renderCard()
    await create()

    expect(await screen.findByText(minted.secret)).toBeInTheDocument()
    // The whole reason this dialog exists rather than a notification.
    expect(screen.getByText(/cannot be shown again/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Copy the key' })).toBeInTheDocument()
  })

  // Forty-odd characters of base64 is not something to select by hand, and a
  // half-selected key is a support question rather than a mistake anyone spots.
  it('copies the key rather than making it be selected by hand', async () => {
    vi.mocked(api.createApiKey).mockResolvedValue(minted)
    await renderCard()
    // userEvent installs the clipboard, so this reads what a real copy put there
    // rather than what the component was asked to put there.
    const user = await create()

    await user.click(await screen.findByRole('button', { name: 'Copy the key' }))

    await waitFor(async () => {
      expect(await navigator.clipboard.readText()).toBe(minted.secret)
    })
  })

  // Once it is closed it is gone: the list it goes back to holds the hint only.
  it('does not leave the key anywhere after the dialog is closed', async () => {
    vi.mocked(api.createApiKey).mockResolvedValue(minted)
    vi.mocked(api.apiKeys).mockResolvedValue([{ ...minted.apiKey, hint: 'Bxv3Qr' }])
    await renderCard()
    const user = await create()

    await user.click(await screen.findByRole('button', { name: 'I have copied it' }))

    await waitFor(() => expect(screen.queryByText(minted.secret)).not.toBeInTheDocument())
    expect(screen.getByText('rf_Bxv3Qr…')).toBeInTheDocument()
  })

  it('defaults to a read key, which is the safer of the two', async () => {
    vi.mocked(api.createApiKey).mockResolvedValue({ ...minted, apiKey: { ...minted.apiKey, scope: 'read' } })
    await renderCard()
    await create()

    await waitFor(() => expect(api.createApiKey).toHaveBeenCalled())
    expect(vi.mocked(api.createApiKey).mock.calls[0][0]).toMatchObject({ name: 'ci', scope: 'read' })
  })

  it('sends an expiry by default, so a forgotten key does not last for ever', async () => {
    vi.mocked(api.createApiKey).mockResolvedValue(minted)
    await renderCard()
    await create()

    await waitFor(() => expect(api.createApiKey).toHaveBeenCalled())
    const sent = vi.mocked(api.createApiKey).mock.calls[0][0]
    expect(sent.expiresAt).toBeTruthy()
    const days = (new Date(sent.expiresAt!).getTime() - Date.now()) / 86400_000
    expect(days).toBeGreaterThan(89)
    expect(days).toBeLessThan(91)
  })

  it('can make a key that never expires', async () => {
    vi.mocked(api.createApiKey).mockResolvedValue(minted)
    await renderCard()
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'New key' }))
    await user.type(await screen.findByLabelText('Name'), 'ci')
    // Mantine's Select labels both the visible input and the hidden one it
    // submits; the first is the one a person clicks.
    await user.click(screen.getAllByLabelText('Expires')[0])
    await user.click(await screen.findByRole('option', { name: 'Never' }))
    await user.click(screen.getByRole('button', { name: 'Create key' }))

    await waitFor(() => expect(api.createApiKey).toHaveBeenCalled())
    expect(vi.mocked(api.createApiKey).mock.calls[0][0].expiresAt).toBeUndefined()
  })

  // Reopening a form still set to admin is how somebody creates an admin key
  // without meaning to.
  it('goes back to a read key after creating an admin one', async () => {
    vi.mocked(api.createApiKey).mockResolvedValue(minted)
    await renderCard()
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'New key' }))
    await user.type(await screen.findByLabelText('Name'), 'ci')
    await user.click(screen.getAllByLabelText('Scope')[0])
    await user.click(await screen.findByRole('option', { name: /^Admin/ }))
    await user.click(screen.getByRole('button', { name: 'Create key' }))

    await user.click(await screen.findByRole('button', { name: 'I have copied it' }))
    await user.click(screen.getByRole('button', { name: 'New key' }))

    expect(await screen.findByLabelText('Name')).toHaveValue('')
    expect(screen.getAllByLabelText('Scope')[0]).toHaveValue('Read — see the fleet, change nothing')
  })

  it('will not create a key with no name to recognise it by', async () => {
    await renderCard()
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'New key' }))
    expect(await screen.findByRole('button', { name: 'Create key' })).toBeDisabled()
  })

  it('reports a key the daemon refused', async () => {
    vi.mocked(api.createApiKey).mockRejectedValue(new Error('api key "ci": already exists'))
    await renderCard()
    await create()

    expect(await screen.findByText('Could not create the key')).toBeInTheDocument()
    expect(screen.getByText(/already exists/)).toBeInTheDocument()
    // And nothing is shown that could be mistaken for a key.
    expect(screen.queryByText(/^rf_/)).not.toBeInTheDocument()
  })
})

describe('revoking a key', () => {
  it('revokes it and says what that means', async () => {
    stored([monitoring])
    await renderCard()
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Revoke monitoring' }))

    await waitFor(() => expect(api.deleteApiKey).toHaveBeenCalledWith(1))
    expect(await screen.findByText(/stops working now/)).toBeInTheDocument()
  })

  it('reads the list again afterwards, so the card shows what is left', async () => {
    stored([monitoring, terraform])
    await renderCard()
    const user = userEvent.setup()

    vi.mocked(api.apiKeys).mockResolvedValue([terraform])
    await user.click(screen.getByRole('button', { name: 'Revoke monitoring' }))

    await waitFor(() => expect(screen.queryByText('monitoring')).not.toBeInTheDocument())
    expect(screen.getByText('terraform')).toBeInTheDocument()
  })

  it('reports a revoke the daemon refused, and keeps the key on screen', async () => {
    stored([monitoring])
    vi.mocked(api.deleteApiKey).mockRejectedValue(new Error('database is locked'))
    await renderCard()
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Revoke monitoring' }))

    expect(await screen.findByText('Could not revoke the key')).toBeInTheDocument()
    expect(screen.getByText('monitoring')).toBeInTheDocument()
  })
})

// The card must not take the page down with it: the password form above works
// regardless, and that is what somebody is on this page for when the daemon is
// struggling to answer at all.
describe('when the keys cannot be read', () => {
  it('shows the card empty rather than never resolving', async () => {
    vi.mocked(api.apiKeys).mockRejectedValue(new Error('database is locked'))
    await renderCard()

    expect(screen.getByText(/Everything calling this daemon is doing so as you/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'New key' })).toBeInTheDocument()
  })
})
