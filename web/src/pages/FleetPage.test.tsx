import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MantineProvider } from '@mantine/core'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { FleetPage } from './FleetPage'
import { pretendNarrow } from '../test-setup'
import { api, type Credential, type Pool, type Runner, type Scale } from '../api'

const credentials: Credential[] = [{ id: 1, name: 'pat', kind: 'pat', hint: '…1234', createdAt: '' }]

// The page draws the activity chart, which asks the daemon for the history as
// soon as it mounts. Left alone that request goes out for real, and lands
// after the test that started it has finished — which is where a run's worth
// of "not wrapped in act(...)" came from. The chart has its own tests; here it
// only has to be quiet.
vi.mock('../api', async () => {
  const actual = await vi.importActual<typeof import('../api')>('../api')
  return { ...actual, api: { ...actual.api, activity: vi.fn() } }
})

beforeEach(() => {
  vi.mocked(api.activity).mockResolvedValue({
    points: [],
    pool: '',
    scope: '',
    scopes: [],
    since: '',
    until: '',
  })
})

async function renderPage(
  runners: Runner[],
  warnings: string[] = [],
  pools: Pool[] = [],
  scaling: Record<string, Scale> = {},
  onChange = vi.fn().mockResolvedValue(undefined),
) {
  const result = render(
    <MantineProvider>
      <FleetPage
        runners={runners}
        pools={pools}
        credentials={credentials}
        scaling={scaling}
        warnings={warnings}
        loading={false}
        onChange={onChange}
      />
    </MantineProvider>,
  )
  // Settled before the test asserts anything: the chart's own state update has
  // to happen while React is still watching, not after the test returns.
  await screen.findByText('No history yet')
  return result
}

afterEach(() => {
  vi.restoreAllMocks()
})

const pool = (over: Partial<Pool> = {}): Pool => ({
  id: 1,
  name: 'web',
  scopeKind: 'repository',
  scope: 'o/r',
  runtime: 'vm',
  nested: false,
  ephemeral: true,
  minReplicas: 1,
  maxReplicas: 4,
  labels: [],
  cpus: 2,
  memoryMb: 4096,
  diskGb: 40,
  image: 'default',
  packages: [],
  recipe: '',
  credentialId: 1,
  enabled: true,
  createdAt: '',
  updatedAt: '',
  ...over,
})

const runner = (over: Partial<Runner> = {}): Runner => ({
  name: 'web-1',
  pool: 'web',
  runtime: 'vm',
  state: 'running',
  job: 'idle',
  generation: 'abc123',
  upToDate: true,
  ...over,
})

describe('FleetPage', () => {
  // An ephemeral runner deregisters itself after every job and a fresh machine
  // boots, so "GitHub has never heard of this one" is most of a busy pool's
  // life. Calling that unknown made a working fleet look broken.
  it('says a runner is starting rather than unknown while it boots', () => {
    renderPage([runner({ job: 'starting' })])
    expect(screen.getByText('starting')).toBeInTheDocument()
  })

  // A runner can be dead and look busy. The dashboard said "running" for a
  // whole afternoon while every machine on the host was crash-looping on a
  // credential it could not read, so a failing runner now says so beside its
  // state, with the command that explains why.
  it('shows a runner that is failing to start, and where to look', async () => {
    await renderPage([
      runner({
        state: 'stopped',
        job: 'unknown',
        trouble: 'failing to start (exit-code), 9 times over; journalctl -u gh-runner@web-1.service',
      }),
    ])

    expect(screen.getByText('failing')).toBeInTheDocument()
  })

  it('says nothing about a runner that is fine', async () => {
    await renderPage([runner()])
    expect(screen.queryByText('failing')).not.toBeInTheDocument()
  })

  it('says what to do when there is nothing yet', async () => {
    await renderPage([])
    expect(screen.getByText('No runners yet')).toBeInTheDocument()
    expect(screen.getByText(/Add a credential, then a pool/)).toBeInTheDocument()
  })

  it('shows what the host says and what GitHub says as separate facts', async () => {
    // A machine that is up with a job on it, and one that is up with nothing:
    // the state column cannot answer the second question.
    await renderPage([
      runner({ name: 'web-1', job: 'busy' }),
      runner({ name: 'web-2', job: 'idle' }),
    ])

    expect(screen.getByText('web-1')).toBeInTheDocument()
    expect(screen.getByText('busy')).toBeInTheDocument()
    expect(screen.getAllByText('running')).toHaveLength(2)
  })

  it('counts the busy runners', async () => {
    await renderPage([runner({ name: 'web-1', job: 'busy' }), runner({ name: 'web-2', job: 'busy' })])
    expect(screen.getByText('Running a job').parentElement).toHaveTextContent('2')
  })

  it('marks a runner that is waiting to be replaced', async () => {
    await renderPage([runner({ upToDate: false })])
    expect(screen.getByText('superseded')).toBeInTheDocument()
  })

  it('shows a draining runner as stopping rather than failed', async () => {
    await renderPage([runner({ state: 'stopping', job: 'busy' })])
    expect(screen.getByText('stopping')).toBeInTheDocument()
  })

  it('says why an elastic pool is the size it is', async () => {
    // A fleet that resizes itself should never leave anyone guessing what it
    // reacted to.
    await renderPage(
      [runner({ job: 'busy' })],
      [],
      [pool()],
      { web: { target: 2, floor: 1, ceiling: 4, reason: 'every runner is busy', scaledUp: true } },
    )

    expect(screen.getByText('every runner is busy')).toBeInTheDocument()
    expect(screen.getByText('1 of 1–4')).toBeInTheDocument()
  })

  it('leaves fixed-size pools out of the scaling panel', async () => {
    await renderPage([runner()], [], [pool({ minReplicas: 2, maxReplicas: 2 })])
    expect(screen.queryByText('Scaling')).not.toBeInTheDocument()
  })

  // The runner is where the trouble shows up, so the pool that decides what it
  // is should be one click away rather than a page away.
  it('opens the definition of the pool a runner belongs to', async () => {
    await renderPage([runner()], [], [pool({ minReplicas: 2, maxReplicas: 2 })])

    await userEvent.click(screen.getByRole('button', { name: 'web' }))

    expect(await screen.findByText('Edit web')).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: /^Name/ })).toHaveValue('web')
    expect(screen.getByRole('textbox', { name: /Maximum runners/ })).toHaveValue('2')
  })

  it('saves a change of replicas made from the fleet', async () => {
    const update = vi.spyOn(api, 'updatePool').mockResolvedValue(pool())
    const onChange = vi.fn().mockResolvedValue(undefined)
    await renderPage([runner()], [], [pool({ minReplicas: 2, maxReplicas: 2 })], {}, onChange)

    await userEvent.click(screen.getByRole('button', { name: 'web' }))
    const maximum = await screen.findByRole('textbox', { name: /Maximum runners/ })
    await userEvent.clear(maximum)
    await userEvent.type(maximum, '5')
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))

    await vi.waitFor(() => expect(update).toHaveBeenCalled())
    expect(update.mock.calls[0][0]).toBe(1)
    expect(update.mock.calls[0][1]).toMatchObject({ minReplicas: 2, maxReplicas: 5 })
    // The fleet reloads, so the row reflects the new bounds without a refresh.
    await vi.waitFor(() => expect(onChange).toHaveBeenCalled())
  })

  // A runner outlives its pool while it drains, and there is no definition
  // left to open for it.
  it('leaves a runner whose pool is gone as plain text', async () => {
    await renderPage([runner({ pool: 'retired' })], [], [pool()])
    expect(screen.queryByRole('button', { name: 'retired' })).not.toBeInTheDocument()
    expect(screen.getByText('retired')).toBeInTheDocument()
  })

  it('surfaces warnings rather than swallowing them', async () => {
    // A fleet that cannot reach GitHub still works, and the operator should be
    // told why the job column is empty.
    await renderPage([runner()], ['pool web: GitHub is unreachable'])
    expect(screen.getByText('pool web: GitHub is unreachable')).toBeInTheDocument()
  })

  // Six columns do not fit on a phone, and the old table did not scroll: it was
  // simply cut off after the third, with no sign that there was more.
  describe('on a phone', () => {
    let restore = () => {}
    afterEach(() => restore())

    it('draws a card per runner instead of a table', async () => {
      restore = pretendNarrow()
      await renderPage([runner({ name: 'web-1', job: 'busy' })])

      expect(screen.queryByRole('table')).not.toBeInTheDocument()
      expect(screen.getByText('web-1')).toBeInTheDocument()
    })

    it('loses none of what the table said', async () => {
      restore = pretendNarrow()
      await renderPage([
        runner({ name: 'web-1', pool: 'web', runtime: 'vm', state: 'stopping', job: 'busy', upToDate: false }),
      ])

      expect(screen.getByText('web-1')).toBeInTheDocument()
      expect(screen.getByText('web')).toBeInTheDocument()
      expect(screen.getByText('vm')).toBeInTheDocument()
      expect(screen.getByText('stopping')).toBeInTheDocument()
      expect(screen.getByText('busy')).toBeInTheDocument()
      expect(screen.getByText('superseded')).toBeInTheDocument()
    })

    it('still says which runner is failing', async () => {
      restore = pretendNarrow()
      await renderPage([runner({ state: 'stopped', job: 'unknown', trouble: 'failing to start' })])
      expect(screen.getByText('failing')).toBeInTheDocument()
    })

    // The way into a pool's definition is on the card as well as in the table:
    // a phone should not be the one place where the runner is a dead end.
    it('opens the pool definition from a card', async () => {
      restore = pretendNarrow()
      await renderPage([runner()], [], [pool({ minReplicas: 2, maxReplicas: 2 })])

      await userEvent.click(screen.getByRole('button', { name: 'web' }))

      expect(await screen.findByText('Edit web')).toBeInTheDocument()
      expect(screen.getByRole('textbox', { name: /^Name/ })).toHaveValue('web')
    })
  })

  it('keeps the table on a wide screen', async () => {
    await renderPage([runner()])
    expect(screen.getByRole('table')).toBeInTheDocument()
  })
})
