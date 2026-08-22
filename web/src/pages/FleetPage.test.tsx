import { render, screen } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { describe, expect, it } from 'vitest'
import { FleetPage } from './FleetPage'
import type { Pool, Runner, Scale } from '../api'

function renderPage(
  runners: Runner[],
  warnings: string[] = [],
  pools: Pool[] = [],
  scaling: Record<string, Scale> = {},
) {
  return render(
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
  it('says what to do when there is nothing yet', () => {
    renderPage([])
    expect(screen.getByText('No runners yet')).toBeInTheDocument()
    expect(screen.getByText(/Add a credential, then a pool/)).toBeInTheDocument()
  })

  it('shows what the host says and what GitHub says as separate facts', () => {
    // A machine that is up with a job on it, and one that is up with nothing:
    // the state column cannot answer the second question.
    renderPage([
      runner({ name: 'web-1', job: 'busy' }),
      runner({ name: 'web-2', job: 'idle' }),
    ])

    expect(screen.getByText('web-1')).toBeInTheDocument()
    expect(screen.getByText('busy')).toBeInTheDocument()
    expect(screen.getAllByText('running')).toHaveLength(2)
  })

  it('counts the busy runners', () => {
    renderPage([runner({ name: 'web-1', job: 'busy' }), runner({ name: 'web-2', job: 'busy' })])
    expect(screen.getByText('Running a job').parentElement).toHaveTextContent('2')
  })

  it('marks a runner that is waiting to be replaced', () => {
    renderPage([runner({ upToDate: false })])
    expect(screen.getByText('superseded')).toBeInTheDocument()
  })

  it('shows a draining runner as stopping rather than failed', () => {
    renderPage([runner({ state: 'stopping', job: 'busy' })])
    expect(screen.getByText('stopping')).toBeInTheDocument()
  })

  it('says why an elastic pool is the size it is', () => {
    // A fleet that resizes itself should never leave anyone guessing what it
    // reacted to.
    renderPage(
      [runner({ job: 'busy' })],
      [],
      [pool()],
      { web: { target: 2, floor: 1, ceiling: 4, reason: 'every runner is busy', scaledUp: true } },
    )

    expect(screen.getByText('every runner is busy')).toBeInTheDocument()
    expect(screen.getByText('1 of 1–4')).toBeInTheDocument()
  })

  it('leaves fixed-size pools out of the scaling panel', () => {
    renderPage([runner()], [], [pool({ minReplicas: 2, maxReplicas: 2 })])
    expect(screen.queryByText('Scaling')).not.toBeInTheDocument()
  })

  it('surfaces warnings rather than swallowing them', () => {
    // A fleet that cannot reach GitHub still works, and the operator should be
    // told why the job column is empty.
    renderPage([runner()], ['pool web: GitHub is unreachable'])
    expect(screen.getByText('pool web: GitHub is unreachable')).toBeInTheDocument()
  })
})
