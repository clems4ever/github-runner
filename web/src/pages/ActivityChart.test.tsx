import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MantineProvider } from '@mantine/core'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ActivityChart } from './ActivityChart'
import { api, type ActivityPoint, type Pool } from '../api'

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

/** A window of history as the daemon sends it. */
function history(points: ActivityPoint[], scopes: string[] = []) {
  return { points, pool: '', scope: '', scopes, since: '', until: '' }
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
  packages: [],
  recipe: '',
  layers: 'off',
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
    activity.mockResolvedValue(history([]))
    renderChart()
    expect(await screen.findByText('No history yet')).toBeInTheDocument()
  })

  it('draws both series, and names them', async () => {
    activity.mockResolvedValue(history(points(30)))
    renderChart()

    // A legend for two series is not optional: identity must never be carried
    // by colour alone.
    expect(await screen.findByText('Running a job')).toBeInTheDocument()
    expect(screen.getByText('Runners')).toBeInTheDocument()
  })

  it('asks for the window the operator chose', async () => {
    activity.mockResolvedValue(history(points(5)))
    renderChart()

    await waitFor(() => expect(activity).toHaveBeenCalledWith(6, undefined, undefined))

    await userEvent.click(screen.getByRole('radio', { name: '24h' }))
    await waitFor(() => expect(activity).toHaveBeenCalledWith(24, undefined, undefined))
  })

  it('can narrow the history to one pool', async () => {
    activity.mockResolvedValue(history(points(5)))
    renderChart([pool('web'), pool('api')])

    await waitFor(() => expect(activity).toHaveBeenCalledWith(6, undefined, undefined))

    await userEvent.click(screen.getByPlaceholderText('All pools'))
    await userEvent.click(await screen.findByRole('option', { name: 'api' }))

    await waitFor(() => expect(activity).toHaveBeenCalledWith(6, 'api', undefined))
  })

  it('offers no pool filter when there is only one', async () => {
    activity.mockResolvedValue(history(points(5)))
    renderChart([pool('web')])
    await screen.findByText('Runners')
    expect(screen.queryByPlaceholderText('All pools')).not.toBeInTheDocument()
  })

  it('can narrow the history to one scope', async () => {
    activity.mockResolvedValue(history(points(5), ['o/api', 'o/web']))
    renderChart([pool('web'), pool('api')])

    await waitFor(() => expect(activity).toHaveBeenCalledWith(6, undefined, undefined))

    await userEvent.click(screen.getByPlaceholderText('All scopes'))
    await userEvent.click(await screen.findByRole('option', { name: 'o/api' }))

    await waitFor(() => expect(activity).toHaveBeenCalledWith(6, undefined, 'o/api'))
  })

  // Several pools can share one scope, and the point of asking by scope is to
  // get the repository's work rather than one pool's share of it. The daemon
  // does the adding up; what matters here is that only the scope is sent.
  it('asks for the scope rather than for the pools sharing it', async () => {
    const [web, spare] = [pool('web'), pool('spare')]
    spare.scope = web.scope
    activity.mockResolvedValue(history(points(5), [web.scope, 'o/api']))
    renderChart([web, spare, pool('api')])

    await userEvent.click(await screen.findByPlaceholderText('All scopes'))
    await userEvent.click(await screen.findByRole('option', { name: web.scope }))

    await waitFor(() => expect(activity).toHaveBeenCalledWith(6, undefined, web.scope))
  })

  // A pool can be deleted; the hours it worked still happened, and the daemon
  // keeps sending the scope for as long as it has history for it.
  it('offers a scope whose pool is gone', async () => {
    activity.mockResolvedValue(history(points(5), ['acme/retired']))
    renderChart([pool('web')])

    await userEvent.click(await screen.findByPlaceholderText('All scopes'))
    await userEvent.click(await screen.findByRole('option', { name: 'acme/retired' }))

    await waitFor(() => expect(activity).toHaveBeenCalledWith(6, undefined, 'acme/retired'))
  })

  it('offers no scope filter when the whole fleet is one scope', async () => {
    activity.mockResolvedValue(history(points(5), ['o/web']))
    renderChart([pool('web')])
    await screen.findByText('Runners')
    expect(screen.queryByPlaceholderText('All scopes')).not.toBeInTheDocument()
  })

  // Asking for one pool and a scope it is not in would narrow the chart to
  // nothing, which reads as an outage rather than as a contradiction.
  it('lets go of a pool that is not in the scope just chosen', async () => {
    activity.mockResolvedValue(history(points(5), ['o/api', 'o/web']))
    renderChart([pool('web'), pool('api')])

    await userEvent.click(await screen.findByPlaceholderText('All pools'))
    await userEvent.click(await screen.findByRole('option', { name: 'api' }))
    await waitFor(() => expect(activity).toHaveBeenCalledWith(6, 'api', undefined))

    await userEvent.click(screen.getByPlaceholderText('All scopes'))
    await userEvent.click(await screen.findByRole('option', { name: 'o/web' }))

    await waitFor(() => expect(activity).toHaveBeenCalledWith(6, undefined, 'o/web'))
    // And the pools left to choose from are the ones that could say something
    // about the scope now being shown.
    await userEvent.click(screen.getByPlaceholderText('All pools'))
    expect(await screen.findByRole('option', { name: 'web' })).toBeInTheDocument()
    expect(screen.queryByRole('option', { name: 'api' })).not.toBeInTheDocument()
  })

  it('names the scope when it has nothing to draw for it', async () => {
    activity.mockResolvedValue(history([], ['o/api', 'o/web']))
    renderChart([pool('web'), pool('api')])

    await userEvent.click(await screen.findByPlaceholderText('All scopes'))
    await userEvent.click(await screen.findByRole('option', { name: 'o/api' }))

    expect(await screen.findByText(/Nothing recorded for o\/api/)).toBeInTheDocument()
  })

  it('says the numbers are peaks, because that is what they are', async () => {
    // A mean over a ten-minute bucket would flatten a two-minute burst into
    // nothing, which is the opposite of what this chart is for.
    activity.mockResolvedValue(history(points(5)))
    renderChart()
    expect(await screen.findByText(/Peak per interval/)).toBeInTheDocument()
  })

  // The grid and the axis labels are coloured through CSS variables rather
  // than the `gridColor` and `textColor` props, which @mantine/charts now
  // forwards to the DOM. Worth pinning: the difference between the two routes
  // is invisible on screen, so a change back to the props would show up only
  // as console noise.
  it('colours the grid and the labels from the palette', async () => {
    activity.mockResolvedValue(history(points(5)))
    const { container } = renderChart()
    await screen.findByText('Runners')

    const root = container.querySelector<HTMLElement>('.mantine-CompositeChart-root')
    expect(root?.style.getPropertyValue('--chart-grid-color')).toBe('#e1e0d9')
    expect(root?.style.getPropertyValue('--chart-text-color')).toBe('#898781')
    expect(root).not.toHaveAttribute('gridcolor')
  })

  it('does not pretend the fleet is empty when it cannot read the history', async () => {
    activity.mockRejectedValue(new Error('unreachable'))
    renderChart()
    expect(await screen.findByText('Could not read the history.')).toBeInTheDocument()
    expect(screen.queryByText('No history yet')).not.toBeInTheDocument()
  })
})
