import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MantineProvider } from '@mantine/core'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ActivityChart } from './ActivityChart'
import { api, type Pool } from '../api'

vi.mock('../api', async () => {
  const actual = await vi.importActual<typeof import('../api')>('../api')
  return { ...actual, api: { ...actual.api, activity: vi.fn() } }
})

const activity = vi.mocked(api.activity)

function points(count: number) {
  const start = Date.parse('2026-08-22T12:00:00Z')
  return Array.from({ length: count }, (_, i) => ({
    at: new Date(start + i * 60_000).toISOString(),
    running: 3,
    busy: i % 3,
  }))
}

const pool = (name: string): Pool => ({
  id: name.length,
  name,
  scopeKind: 'repository',
  scope: `o/${name}`,
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
})

function renderChart(pools: Pool[] = []) {
  return render(
    <MantineProvider>
      <ActivityChart pools={pools} />
    </MantineProvider>,
  )
}

describe('ActivityChart', () => {
  beforeEach(() => {
    activity.mockReset()
  })

  it('says so when there is no history yet', async () => {
    activity.mockResolvedValue({ points: [], pool: '', since: '', until: '' })
    renderChart()
    expect(await screen.findByText('No history yet')).toBeInTheDocument()
  })

  it('draws both series, and names them', async () => {
    activity.mockResolvedValue({ points: points(30), pool: '', since: '', until: '' })
    renderChart()

    // A legend for two series is not optional: identity must never be carried
    // by colour alone.
    expect(await screen.findByText('Running a job')).toBeInTheDocument()
    expect(screen.getByText('Runners')).toBeInTheDocument()
  })

  it('asks for the window the operator chose', async () => {
    activity.mockResolvedValue({ points: points(5), pool: '', since: '', until: '' })
    renderChart()

    await waitFor(() => expect(activity).toHaveBeenCalledWith(6, undefined))

    await userEvent.click(screen.getByRole('radio', { name: '24h' }))
    await waitFor(() => expect(activity).toHaveBeenCalledWith(24, undefined))
  })

  it('can narrow the history to one pool', async () => {
    activity.mockResolvedValue({ points: points(5), pool: '', since: '', until: '' })
    renderChart([pool('web'), pool('api')])

    await waitFor(() => expect(activity).toHaveBeenCalledWith(6, undefined))

    await userEvent.click(screen.getByPlaceholderText('All pools'))
    await userEvent.click(await screen.findByRole('option', { name: 'api' }))

    await waitFor(() => expect(activity).toHaveBeenCalledWith(6, 'api'))
  })

  it('offers no pool filter when there is only one', async () => {
    activity.mockResolvedValue({ points: points(5), pool: '', since: '', until: '' })
    renderChart([pool('web')])
    await screen.findByText('Runners')
    expect(screen.queryByPlaceholderText('All pools')).not.toBeInTheDocument()
  })

  it('says the numbers are peaks, because that is what they are', async () => {
    // A mean over a ten-minute bucket would flatten a two-minute burst into
    // nothing, which is the opposite of what this chart is for.
    activity.mockResolvedValue({ points: points(5), pool: '', since: '', until: '' })
    renderChart()
    expect(await screen.findByText(/Peak per interval/)).toBeInTheDocument()
  })

  it('does not pretend the fleet is empty when it cannot read the history', async () => {
    activity.mockRejectedValue(new Error('unreachable'))
    renderChart()
    expect(await screen.findByText('Could not read the history.')).toBeInTheDocument()
    expect(screen.queryByText('No history yet')).not.toBeInTheDocument()
  })
})
