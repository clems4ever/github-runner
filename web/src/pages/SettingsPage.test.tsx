import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MantineProvider } from '@mantine/core'
import { Notifications, notifications } from '@mantine/notifications'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { SettingsPage } from './SettingsPage'
import { api, type Budget } from '../api'

vi.mock('../api', async () => {
  const actual = await vi.importActual<typeof import('../api')>('../api')
  return {
    ...actual,
    api: { ...actual.api, settings: vi.fn(), setBudget: vi.fn(), setPassword: vi.fn() },
  }
})

const uncapped: Budget = { cpus: 0, cpuWeight: 0, memoryMb: 0, hardMemory: false }

function stored(budget: Partial<Budget> = {}) {
  vi.mocked(api.settings).mockResolvedValue({
    authUser: 'admin',
    version: 'test',
    budget: { ...uncapped, ...budget },
  })
}

async function renderPage() {
  render(
    <MantineProvider>
      <Notifications />
      <SettingsPage health={{ status: 'ok', version: 'test', configured: true }} />
    </MantineProvider>,
  )
  // The budget is fetched on mount; nothing about it can be asserted until it
  // has arrived, and a state update after the test returns is a warning nobody
  // reads.
  await screen.findByText('Fleet budget')
}

beforeEach(() => {
  // Mantine keeps notifications in a store outside React, so one raised by the
  // previous test is still on screen for the next one.
  notifications.clean()
  vi.mocked(api.setBudget).mockImplementation(async (budget) => budget)
  stored()
})

describe('the fleet budget', () => {
  // Zero is how a dimension is switched off, and the form says so rather than
  // leaving the box empty: an empty box is a question about whether the page
  // loaded.
  it('shows an uncapped fleet as zeroes that say what zero means', async () => {
    await renderPage()

    expect(screen.getByLabelText('CPU')).toHaveValue('0')
    expect(screen.getByLabelText('Memory (MiB)')).toHaveValue('0')
    expect(screen.getAllByText(/0 for no cap/)).toHaveLength(2)
  })

  it('shows a budget that has been set', async () => {
    stored({ cpus: 12, memoryMb: 24576, cpuWeight: 50 })
    await renderPage()

    expect(screen.getByLabelText('CPU')).toHaveValue('12')
    expect(screen.getByLabelText('Memory (MiB)')).toHaveValue('24576')
    expect(screen.getByLabelText(/Share when the host is contended/)).toHaveValue('50')
  })

  it('saves what was typed', async () => {
    await renderPage()
    const user = userEvent.setup()

    await user.clear(screen.getByLabelText('CPU'))
    await user.type(screen.getByLabelText('CPU'), '6')
    await user.click(screen.getByRole('button', { name: 'Save budget' }))

    await waitFor(() => expect(api.setBudget).toHaveBeenCalled())
    expect(vi.mocked(api.setBudget).mock.calls[0][0]).toMatchObject({ cpus: 6 })
  })

  // The thing an operator most needs to be told after saving, because it is the
  // opposite of what they would assume from every other setting on this page:
  // it does not wait for the next machine.
  it('says that a saved budget reaches the machines already running', async () => {
    await renderPage()
    const user = userEvent.setup()

    await user.clear(screen.getByLabelText('CPU'))
    await user.type(screen.getByLabelText('CPU'), '6')
    await user.click(screen.getByRole('button', { name: 'Save budget' }))

    expect(await screen.findByText(/already running/)).toBeInTheDocument()
  })

  // And when the cap is removed, it says that instead — "it applies to the
  // machines already running" is a strange thing to read about nothing at all.
  it('says when the fleet has been uncapped', async () => {
    stored({ cpus: 8 })
    await renderPage()
    const user = userEvent.setup()

    await user.clear(screen.getByLabelText('CPU'))
    await user.type(screen.getByLabelText('CPU'), '0')
    await user.click(screen.getByRole('button', { name: 'Save budget' }))

    expect(await screen.findByText(/no longer capped/)).toBeInTheDocument()
  })

  // The kernel's out-of-memory killer needs a ceiling to sit above, and the
  // daemon refuses a hard limit without one. The form does not let it be asked
  // for in the first place.
  it('will not offer to kill machines until there is a memory ceiling', async () => {
    await renderPage()
    expect(screen.getByLabelText(/Kill a machine/)).toBeDisabled()
  })

  it('offers it once memory is capped', async () => {
    stored({ memoryMb: 8192 })
    await renderPage()
    expect(screen.getByLabelText(/Kill a machine/)).toBeEnabled()
  })

  // The setting that can cost somebody their job says so where it is turned on,
  // not only in the documentation.
  it('says what turning the killer on costs', async () => {
    stored({ memoryMb: 8192 })
    await renderPage()
    expect(screen.getByText(/costs somebody/)).toBeInTheDocument()
  })

  // A daemon that cannot answer must not take the password form down with it:
  // that is what somebody would be on this page for when things are going badly.
  it('still offers the password form when the budget cannot be read', async () => {
    vi.mocked(api.settings).mockRejectedValue(new Error('database is locked'))
    render(
      <MantineProvider>
        <Notifications />
        <SettingsPage health={{ status: 'ok', version: 'test', configured: true }} />
      </MantineProvider>,
    )

    expect(await screen.findByText('Web access')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Change password' })).toBeInTheDocument()
  })

  it('reports a budget the daemon refused', async () => {
    vi.mocked(api.setBudget).mockRejectedValue(new Error('memory 8 MiB: want 512 to 67108864'))
    await renderPage()
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Save budget' }))

    expect(await screen.findByText('Could not save the budget')).toBeInTheDocument()
    expect(screen.getByText(/want 512/)).toBeInTheDocument()
  })
})
