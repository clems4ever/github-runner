import { render, screen } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { describe, expect, it } from 'vitest'
import { FleetPage } from './FleetPage'
import type { Pool, Runner } from '../api'

function renderPage(runners: Runner[], warnings: string[] = [], pools: Pool[] = []) {
  return render(
    <MantineProvider>
      <FleetPage runners={runners} pools={pools} warnings={warnings} loading={false} />
    </MantineProvider>,
  )
}

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

  it('surfaces warnings rather than swallowing them', () => {
    // A fleet that cannot reach GitHub still works, and the operator should be
    // told why the job column is empty.
    renderPage([runner()], ['pool web: GitHub is unreachable'])
    expect(screen.getByText('pool web: GitHub is unreachable')).toBeInTheDocument()
  })
})
