import { render, screen } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ResourcesPage, bytes } from './ResourcesPage'
import { pretendNarrow } from '../test-setup'
import { api, type ResourceReport } from '../api'

// The page draws the host chart, which asks the daemon for the history as soon
// as it mounts. Left alone that request goes out for real and lands after the
// test that started it has finished. The chart has its own concerns; here it
// only has to be quiet.
vi.mock('../api', async () => {
  const actual = await vi.importActual<typeof import('../api')>('../api')
  return { ...actual, api: { ...actual.api, resourceHistory: vi.fn() } }
})

beforeEach(() => {
  vi.mocked(api.resourceHistory).mockResolvedValue({ points: [], since: '', until: '' })
})

const report = (over: Partial<ResourceReport> = {}): ResourceReport => ({
  ready: true,
  at: '2026-08-22T12:00:00Z',
  host: {
    cpus: 8,
    cpuPercent: 42.5,
    memoryUsedBytes: 4 * 1024 ** 3,
    memoryTotalBytes: 16 * 1024 ** 3,
    diskPath: '/var/lib/runner-fleet',
    diskUsedBytes: 100 * 1024 ** 3,
    diskTotalBytes: 400 * 1024 ** 3,
    load1: 1.5,
    load5: 1,
    load15: 0.5,
  },
  runners: [],
  warnings: [],
  committed: { runners: 4, cpus: 8, memoryBytes: 16 * 1024 ** 3, diskBytes: 160 * 1024 ** 3 },
  ...over,
})

async function renderPage(value: ResourceReport | null) {
  const result = render(
    <MantineProvider>
      <ResourcesPage report={value} />
    </MantineProvider>,
  )
  // Settled before the test asserts anything: the chart's own state update has
  // to happen while React is still watching, not after the test returns. There
  // is no chart at all before the first reading, so there is nothing to wait
  // for either.
  if (value?.ready) await screen.findByText('No history yet')
  return result
}

describe('ResourcesPage', () => {
  // A host with no processors and no memory is not a measurement. For the first
  // second after a restart there is nothing to draw, and saying so beats
  // drawing an empty machine.
  it('says it is still taking the first reading', async () => {
    await renderPage({ ready: false })
    expect(screen.getByText(/Taking the first reading/)).toBeInTheDocument()
    expect(screen.queryByText('CPU')).not.toBeInTheDocument()
  })

  it('shows what the host is using, in units and not only in bars', async () => {
    await renderPage(report())

    expect(screen.getByText('43%')).toBeInTheDocument()
    expect(screen.getByText('8 cores')).toBeInTheDocument()
    expect(screen.getByText('4.0 GB of 16.0 GB')).toBeInTheDocument()
    // The filesystem is named, because "disk" on a host with several is not an
    // answer.
    expect(screen.getByText('/var/lib/runner-fleet')).toBeInTheDocument()
  })

  it('shows what each runner is using, whatever runtime it is', async () => {
    await renderPage(
      report({
        runners: [
          { name: 'web-1', pool: 'web', runtime: 'vm', cpuPercent: 12.5, memoryBytes: 2 * 1024 ** 3 },
          {
            name: 'api-1',
            pool: 'api',
            runtime: 'container',
            cpuPercent: null,
            memoryBytes: 512 * 1024 ** 2,
          },
        ],
      }),
    )

    expect(screen.getByText('web-1')).toBeInTheDocument()
    expect(screen.getByText('12.5%')).toBeInTheDocument()
    expect(screen.getByText('2.0 GB')).toBeInTheDocument()
    expect(screen.getByText('512 MB')).toBeInTheDocument()
  })

  // A rate needs two readings. A dash is the truth; a zero would be a machine
  // that is busily booting shown as doing nothing.
  it('leaves a runner it has only seen once blank rather than idle', async () => {
    await renderPage(
      report({
        runners: [{ name: 'web-1', pool: 'web', runtime: 'vm', cpuPercent: null, memoryBytes: 1024 }],
      }),
    )

    expect(screen.getByText('—')).toBeInTheDocument()
    expect(screen.queryByText('0.0%')).not.toBeInTheDocument()
  })

  it('says what the pools have promised, next to what the host has', async () => {
    await renderPage(report())
    expect(screen.getByText('Committed at full stretch')).toBeInTheDocument()
    expect(screen.getByText('8 of 8')).toBeInTheDocument()
  })

  // Oversubscription is not an error — pools rarely peak together — but it is a
  // fact the operator should be able to read without doing the arithmetic.
  it('flags pools that between them promise more than the host has', async () => {
    await renderPage(
      report({
        committed: { runners: 16, cpus: 32, memoryBytes: 64 * 1024 ** 3, diskBytes: 0 },
      }),
    )

    expect(screen.getByText('32 of 8')).toBeInTheDocument()
    // The word as well as the colour: a reader who cannot tell orange from grey
    // still reads it.
    expect(screen.getAllByText('more than the host has')).toHaveLength(2)
  })

  // A host that has never set a budget has nothing to say about one, and a card
  // of zeroes reading "uncapped, uncapped, uncapped" is a row somebody has to
  // learn to ignore.
  it('says nothing about a budget nobody set', async () => {
    await renderPage(report({ budget: { cpus: 0, cpuWeight: 0, memoryMb: 0, diskGb: 0, hardMemory: false } }))
    expect(screen.queryByText('Fleet budget')).not.toBeInTheDocument()
  })

  it('shows the budget against the host once there is one', async () => {
    await renderPage(
      report({ budget: { cpus: 4, cpuWeight: 0, memoryMb: 8192, diskGb: 0, hardMemory: false } }),
    )

    expect(screen.getByText('Fleet budget')).toBeInTheDocument()
    expect(screen.getByText('4 of 8')).toBeInTheDocument()
    expect(screen.getByText('8.0 GB of 16.0 GB')).toBeInTheDocument()
  })

  // The pools are configured for the busiest hour and the budget is what keeps
  // that hour from taking the host, so a commitment above the budget is the
  // ordinary way to run this. What the operator needs to know is the
  // consequence: the pools will not reach their maximums.
  it('says when the budget will hold the pools below their maximums', async () => {
    await renderPage(
      report({
        committed: { runners: 8, cpus: 32, memoryBytes: 64 * 1024 ** 3, diskBytes: 0 },
        budget: { cpus: 4, cpuWeight: 0, memoryMb: 8192, diskGb: 0, hardMemory: false },
      }),
    )

    expect(screen.getAllByText('pools held below their maximums')).toHaveLength(2)
  })

  // The one setting that can cost somebody a job, said on the page they would
  // read afterwards looking for a reason that is not in the job's own log.
  it('says out loud when the fleet is set to kill a machine at the ceiling', async () => {
    await renderPage(
      report({ budget: { cpus: 0, cpuWeight: 0, memoryMb: 8192, diskGb: 0, hardMemory: true } }),
    )

    expect(screen.getByText('a machine is killed')).toBeInTheDocument()
    expect(screen.getByText('mid-job')).toBeInTheDocument()
  })

  // A weight is not a cap, and a fleet that only yields under contention is
  // still allowed the whole host.
  it('shows a share without claiming anything is capped', async () => {
    await renderPage(
      report({ budget: { cpus: 0, cpuWeight: 20, memoryMb: 0, diskGb: 0, hardMemory: false } }),
    )

    expect(screen.getByText('Share when contended')).toBeInTheDocument()
    expect(screen.getByText('20')).toBeInTheDocument()
    expect(screen.getAllByText('uncapped')).toHaveLength(3)
  })

  // Disk is the dimension a host actually runs out of, and the page somebody
  // opens when it has has to show what the ceiling was.
  it('shows the disk ceiling against what the filesystem has', async () => {
    await renderPage(
      report({ budget: { cpus: 0, cpuWeight: 0, memoryMb: 0, diskGb: 200, hardMemory: false } }),
    )

    expect(screen.getAllByText('Disk').length).toBeGreaterThan(0)
    expect(screen.getByText(/200 GB of/)).toBeInTheDocument()
  })

  // Containers are not in the group the budget is enforced in, and a host
  // running both would otherwise read this as a fleet-wide ceiling.
  it('says which runtime the budget covers', async () => {
    await renderPage(
      report({ budget: { cpus: 4, cpuWeight: 0, memoryMb: 0, diskGb: 0, hardMemory: false } }),
    )
    expect(screen.getByText(/Machine pools only/)).toBeInTheDocument()
  })

  it('surfaces a runtime that could not be measured rather than showing it as empty', async () => {
    await renderPage(report({ warnings: ['is dockerd running?'] }))
    expect(screen.getByText('is dockerd running?')).toBeInTheDocument()
  })

  it('says when the host has nothing on it', async () => {
    await renderPage(report())
    expect(screen.getByText('Nothing is running on this host yet.')).toBeInTheDocument()
  })

  // Five columns of usage do not fit on a phone, and the table did not scroll:
  // memory, the number most worth looking at, was off the right-hand edge.
  describe('on a phone', () => {
    let restore = () => {}
    afterEach(() => restore())

    const busy = report({
      runners: [
        { name: 'web-1', pool: 'web', runtime: 'vm', cpuPercent: 12.5, memoryBytes: 2 * 1024 ** 3 },
      ],
    })

    it('draws a card per runner instead of a table', async () => {
      restore = pretendNarrow()
      await renderPage(busy)

      expect(screen.queryByRole('table')).not.toBeInTheDocument()
      expect(screen.getByText('web-1')).toBeInTheDocument()
      expect(screen.getByText('12.5%')).toBeInTheDocument()
      expect(screen.getByText('2.0 GB')).toBeInTheDocument()
    })

    it('still says a reading is not in yet rather than calling it zero', async () => {
      restore = pretendNarrow()
      await renderPage(
        report({
          runners: [
            { name: 'web-1', pool: 'web', runtime: 'vm', cpuPercent: null, memoryBytes: 1024 ** 3 },
          ],
        }),
      )

      expect(screen.getByText('—')).toBeInTheDocument()
      expect(screen.queryByText('0.0%')).not.toBeInTheDocument()
    })
  })

  it('keeps the table on a wide screen', async () => {
    await renderPage(
      report({
        runners: [
          { name: 'web-1', pool: 'web', runtime: 'vm', cpuPercent: 12.5, memoryBytes: 1024 ** 3 },
        ],
      }),
    )
    expect(screen.getByRole('table')).toBeInTheDocument()
  })
})

describe('bytes', () => {
  // Powers of 1024 labelled GB, because that is what every other number on this
  // host is: a pool asking for 4096 MB gets 4096 MiB, and the meters have to
  // agree with the pool editor.
  it('uses the units the rest of the fleet is configured in', () => {
    expect(bytes(4 * 1024 ** 3)).toBe('4.0 GB')
    expect(bytes(1024)).toBe('1.0 KB')
    expect(bytes(512)).toBe('512 B')
    expect(bytes(0)).toBe('0 B')
  })
})
