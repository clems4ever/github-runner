import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MantineProvider } from '@mantine/core'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { JobsPage, duration } from './JobsPage'
import { api, type JobDay, type Pool, type PoolJobs } from '../api'
import { pretendNarrow } from '../test-setup'

vi.mock('../api', async () => {
  const actual = await vi.importActual<typeof import('../api')>('../api')
  return { ...actual, api: { ...actual.api, jobs: vi.fn() } }
})

const jobs = vi.mocked(api.jobs)

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
  sleeps: false,
  credentialId: 1,
  enabled: true,
  createdAt: '',
  updatedAt: '',
})

function answer(pools: PoolJobs[], days: JobDay[]) {
  jobs.mockResolvedValue({ pools, days, since: '', until: '' })
}

function renderPage(pools: Pool[] = []) {
  return render(
    <MantineProvider>
      <JobsPage pools={pools} />
    </MantineProvider>,
  )
}

// Every pool name is on the page twice: once in the table and once as an option
// in the filter above it. The assertions here are about the table.
const table = () => within(screen.getByRole('table'))

describe('JobsPage', () => {
  beforeEach(() => {
    jobs.mockReset()
  })

  it('says so when nothing has been run yet', async () => {
    answer([], [])
    renderPage([pool('web')])
    expect(await screen.findByText('Nothing run yet')).toBeInTheDocument()
  })

  it('reports what each pool ran and what it cost in runner-time', async () => {
    answer(
      [
        { pool: 'web', jobs: 40, seconds: 36000 },
        { pool: 'api', jobs: 10, seconds: 1800 },
      ],
      [
        { day: '2026-08-20', pool: 'web', jobs: 15, seconds: 20000 },
        { day: '2026-08-21', pool: 'web', jobs: 25, seconds: 16000 },
        { day: '2026-08-21', pool: 'api', jobs: 10, seconds: 1800 },
      ],
    )
    renderPage([pool('web'), pool('api')])

    await screen.findByRole('table')
    expect(table().getByText('web')).toBeInTheDocument()
    expect(table().getByText('40')).toBeInTheDocument()
    expect(table().getByText('10.0 h')).toBeInTheDocument()
    // The whole window, added up across the pools, at the top of the page.
    expect(screen.getByText('50')).toBeInTheDocument()
    expect(screen.getByText('10.5 h')).toBeInTheDocument()
  })

  // Neither figure is exact and the page has to say so, once, rather than let a
  // reader take a sampled count for GitHub's own record.
  it('admits the counts are observed rather than reported', async () => {
    answer([], [])
    renderPage()
    expect(await screen.findByText(/Counted by watching runners/)).toBeInTheDocument()
  })

  // The clearest possible case for making a pool smaller, and it would
  // otherwise be the one row missing from the table.
  it('lists a pool that ran nothing rather than leaving it out', async () => {
    answer([{ pool: 'web', jobs: 4, seconds: 600 }], [{ day: '2026-08-21', pool: 'web', jobs: 4, seconds: 600 }])
    renderPage([pool('web'), pool('idle')])

    await screen.findByRole('table')
    expect(table().getByText('idle')).toBeInTheDocument()
    expect(table().getByText('0s')).toBeInTheDocument()
  })

  // The host paid for a deleted pool's work whether or not the pool still
  // exists, which is most of what an audit is for.
  it('keeps a deleted pool in the tally and marks it as gone', async () => {
    answer(
      [{ pool: 'retired', jobs: 9, seconds: 900 }],
      [{ day: '2026-08-21', pool: 'retired', jobs: 9, seconds: 900 }],
    )
    renderPage([pool('web')])

    await screen.findByRole('table')
    expect(table().getByText('retired')).toBeInTheDocument()
    expect(table().getByText('deleted')).toBeInTheDocument()
  })

  // The busiest day is the one that cost the most runner-time, which is not the
  // one with the most jobs: forty short jobs are a quieter day than fifteen long
  // ones, and it is the long ones a pool has to be big enough for.
  it('names the day a pool worked hardest', async () => {
    answer(
      [{ pool: 'web', jobs: 40, seconds: 36000 }],
      [
        { day: '2026-08-20', pool: 'web', jobs: 15, seconds: 20000 },
        { day: '2026-08-21', pool: 'web', jobs: 25, seconds: 16000 },
      ],
    )
    renderPage([pool('web')])

    await screen.findByRole('table')
    // Written in whatever the browser's locale calls that date, so the day is
    // asserted rather than the wording.
    const busiest = new Date('2026-08-20T00:00:00Z').toLocaleDateString([], {
      day: 'numeric',
      month: 'short',
    })
    expect(table().getByText(busiest)).toBeInTheDocument()
    expect(table().getByText('5.6 h')).toBeInTheDocument()
  })

  it('narrows to one pool', async () => {
    answer(
      [
        { pool: 'web', jobs: 40, seconds: 36000 },
        { pool: 'api', jobs: 10, seconds: 1800 },
      ],
      [
        { day: '2026-08-21', pool: 'web', jobs: 40, seconds: 36000 },
        { day: '2026-08-21', pool: 'api', jobs: 10, seconds: 1800 },
      ],
    )
    renderPage([pool('web'), pool('api')])

    await screen.findByRole('table')
    await userEvent.click(screen.getByPlaceholderText('All pools'))
    await userEvent.click(await screen.findByRole('option', { name: 'api' }))

    await waitFor(() => expect(table().queryByText('web')).not.toBeInTheDocument())
    // The heading row and one pool.
    expect(table().getAllByRole('row')).toHaveLength(2)
    // The summary above follows the filter down rather than going on totalling
    // a fleet the reader can no longer see: api's ten jobs, twice over.
    expect(screen.getAllByText('10')).toHaveLength(2)
  })

  it('asks for a longer window when one is chosen', async () => {
    answer([], [])
    renderPage()
    await screen.findByText('Nothing run yet')

    await userEvent.click(screen.getByText('90d'))
    await waitFor(() => expect(jobs).toHaveBeenCalledWith(90))
  })

  it('says the tally could not be read without implying the fleet is down', async () => {
    jobs.mockRejectedValue(new Error('nope'))
    renderPage()
    expect(await screen.findByText('Could not read the tally.')).toBeInTheDocument()
  })

  describe('on a narrow screen', () => {
    let restore = () => {}
    afterEach(() => restore())

    // Five columns will not fit on a phone, and a clipped table hides the
    // figure the page exists to show rather than saying it is hidden.
    it('draws a card per pool instead of a table', async () => {
      restore = pretendNarrow()
      answer(
        [{ pool: 'web', jobs: 40, seconds: 36000 }],
        [{ day: '2026-08-21', pool: 'web', jobs: 40, seconds: 36000 }],
      )
      renderPage([pool('web')])

      // "Busiest day" is a column heading and nothing else on the page, so it
      // only appears once the row has been redrawn as a card.
      expect(await screen.findByText('Busiest day')).toBeInTheDocument()
      expect(screen.queryByRole('table')).not.toBeInTheDocument()
      expect(screen.getByText('web')).toBeInTheDocument()
      expect(screen.getAllByText('15 min').length).toBeGreaterThan(0)
    })
  })
})

describe('duration', () => {
  // One unit, never two. These are sampled figures, and writing them to the
  // minute would claim a precision the daemon cannot see.
  it('uses the unit somebody would say the span in', () => {
    expect(duration(0)).toBe('0s')
    expect(duration(45)).toBe('45s')
    expect(duration(1800)).toBe('30 min')
    expect(duration(36000)).toBe('10.0 h')
    expect(duration(3600 * 250)).toBe('250 h')
  })
})
