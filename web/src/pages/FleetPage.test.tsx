import { render, screen } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { FleetPage } from './FleetPage'
import { api, type Pool, type Runner, type Scale } from '../api'

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
  vi.mocked(api.activity).mockResolvedValue({ points: [], pool: '', since: '', until: '' })
})

async function renderPage(
  runners: Runner[],
  warnings: string[] = [],
  pools: Pool[] = [],
  scaling: Record<string, Scale> = {},
) {
  const result = render(
    <MantineProvider>
      <FleetPage
        runners={runners}
        pools={pools}
        scaling={scaling}
        warnings={warnings}
        loading={false}
      />
    </MantineProvider>,
  )
  // Settled before the test asserts anything: the chart's own state update has
  // to happen while React is still watching, not after the test returns.
  await screen.findByText('No history yet')
  return result
}

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

  it('surfaces warnings rather than swallowing them', async () => {
    // A fleet that cannot reach GitHub still works, and the operator should be
    // told why the job column is empty.
    await renderPage([runner()], ['pool web: GitHub is unreachable'])
    expect(screen.getByText('pool web: GitHub is unreachable')).toBeInTheDocument()
  })
})
