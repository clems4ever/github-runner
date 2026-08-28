import { render, screen } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { describe, expect, it } from 'vitest'
import { ImageBuilds, duration } from './ImageBuilds'
import type { ImageBuild } from '../api'

function build(overrides: Partial<ImageBuild> = {}): ImageBuild {
  return {
    image: 'runner-fleet-noble-abc123',
    pool: 'web',
    runner: 'web-1',
    phase: 'running',
    startedAt: '2026-08-28T12:00:00Z',
    seconds: 252,
    ...overrides,
  }
}

function show(builds: ImageBuild[]) {
  return render(
    <MantineProvider>
      <ImageBuilds builds={builds} />
    </MantineProvider>,
  )
}

describe('ImageBuilds', () => {
  // The panel is above the fleet table, so on the ordinary day where nothing
  // is building it has to take up no room at all.
  it('says nothing when nothing is building', () => {
    show([])
    expect(screen.queryAllByTestId('image-build')).toHaveLength(0)
  })

  it('names the pool whose image is building, and how long it has been', () => {
    show([build()])
    expect(screen.getByText(/Building the golden image for web/)).toBeInTheDocument()
    expect(screen.getByText('4m 12s')).toBeInTheDocument()
  })

  // The whole reason this exists: a progress display that says what is
  // happening beats one that says only that something is.
  it("shows the build's own last words", () => {
    show([build({ detail: 'Setting up nftables (1.0.9-1build1) ...' })])
    expect(screen.getByText('Setting up nftables (1.0.9-1build1) ...')).toBeInTheDocument()
  })

  // The first build on a host spends minutes downloading before any machine
  // boots, and saying "this happens once" is the difference between waiting
  // and rebooting the host.
  it('explains the download the first build starts with', () => {
    show([build({ phase: 'fetching', detail: 'fetching the Ubuntu image, 310 MB so far' })])
    expect(screen.getByText(/happens once per host/)).toBeInTheDocument()
    expect(screen.getByText('fetching the Ubuntu image, 310 MB so far')).toBeInTheDocument()
  })

  it('says when a build has stopped saying anything', () => {
    show([build({ silent: true })])
    expect(screen.getByText(/nothing printed for a while/)).toBeInTheDocument()
  })

  // A failed build is the case the panel exists for. It has to say what went
  // wrong, where the rest of it is, and what the pool is doing meanwhile —
  // otherwise the fleet page shows an empty pool and no reason.
  it('reports a failure with its error and its console', () => {
    show([
      build({
        phase: 'failed',
        error: 'the image build did not report that it finished (a recipe that exits non-zero fails the build)',
        console: '/var/lib/runner-fleet/images/last-build-console.log',
        seconds: 362,
      }),
    ])
    expect(screen.getByText(/did not build/)).toBeInTheDocument()
    expect(screen.getByText(/a recipe that exits non-zero fails the build/)).toBeInTheDocument()
    expect(
      screen.getByText('/var/lib/runner-fleet/images/last-build-console.log'),
    ).toBeInTheDocument()
    expect(screen.getByText(/keeps running the image it already had/)).toBeInTheDocument()
  })

  // Most of the agent's errors name the console themselves, because the same
  // message goes to the journal where there is no field to put a path in. Said
  // twice, on two lines, it reads like two different files.
  it('does not print the console path twice', () => {
    const path = '/var/lib/runner-fleet/images/last-build-console.log'
    show([
      build({
        phase: 'failed',
        error: `the image build failed; the console is at ${path}: exit status 1`,
        console: path,
      }),
    ])
    expect(screen.getAllByText(new RegExp(path.replace(/[/.]/g, '\\$&')))).toHaveLength(1)
  })

  it('says a build worked, and what happens next', () => {
    show([build({ phase: 'done', seconds: 372 })])
    expect(screen.getByText(/Built the golden image for web in 6m 12s/)).toBeInTheDocument()
    expect(screen.getByText(/replaced as they finish their jobs/)).toBeInTheDocument()
  })

  it('shows one line per pool', () => {
    show([build(), build({ pool: 'ci', runner: 'ci-1', phase: 'failed', error: 'no' })])
    expect(screen.getAllByTestId('image-build')).toHaveLength(2)
  })
})

describe('duration', () => {
  it('reads the way somebody would say it', () => {
    expect(duration(0)).toBe('0s')
    expect(duration(59)).toBe('59s')
    expect(duration(60)).toBe('1m 0s')
    expect(duration(252)).toBe('4m 12s')
    expect(duration(3600)).toBe('1h 0m')
    expect(duration(3900)).toBe('1h 5m')
  })

  // Clocks disagree: the daemon computes elapsed time from a record another
  // process wrote, and a machine whose clock stepped backwards would otherwise
  // put "-3s" on the page.
  it('does not show negative time', () => {
    expect(duration(-3)).toBe('0s')
  })
})
