import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MantineProvider } from '@mantine/core'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ImageBadge, PoolImagePanel, duration } from './PoolImage'
import { api, type ImageBuild, type Pool, type PoolImage } from '../api'

vi.mock('../api', async () => {
  const actual = await vi.importActual<typeof import('../api')>('../api')
  return {
    ...actual,
    api: {
      ...actual.api,
      poolImage: vi.fn(),
      buildPoolImage: vi.fn(),
      imageBuildLog: vi.fn(),
    },
  }
})

const poolImage = vi.mocked(api.poolImage)
const buildPoolImage = vi.mocked(api.buildPoolImage)
const imageBuildLog = vi.mocked(api.imageBuildLog)

const POOL = { id: 1, name: 'web', runtime: 'vm' } as Pool

const build = (over: Partial<ImageBuild> = {}): ImageBuild => ({
  id: 3,
  pool: 'web',
  image: 'runner-noble-default-abc123',
  phase: 'running',
  trigger: 'automatic',
  startedAt: '2026-08-28T12:00:00Z',
  seconds: 252,
  hasLog: true,
  ...over,
})

const status = (over: Partial<PoolImage> = {}): PoolImage => ({
  pool: 'web',
  image: 'runner-noble-default-abc123',
  state: 'building',
  ready: false,
  summary: 'its image has been building for 4m 12s',
  ...over,
})

function showPanel() {
  return render(
    <MantineProvider>
      <PoolImagePanel pool={POOL} opened onClose={() => {}} />
    </MantineProvider>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  imageBuildLog.mockResolvedValue('==> building runner-noble-default-abc123\n')
})

describe('ImageBadge', () => {
  // The signal the whole change exists for: one word per pool for whether it
  // can take a job at all.
  it('says a pool whose image is built is built', () => {
    render(
      <MantineProvider>
        <ImageBadge status={status({ state: 'ready', ready: true })} onOpen={() => {}} />
      </MantineProvider>,
    )
    expect(screen.getByTestId('image-badge')).toHaveTextContent('built')
  })

  it('says how long a build has been going', () => {
    render(
      <MantineProvider>
        <ImageBadge status={status({ build: build() })} onOpen={() => {}} />
      </MantineProvider>,
    )
    expect(screen.getByTestId('image-badge')).toHaveTextContent('building 4m 12s')
  })

  it('says when a build failed', () => {
    render(
      <MantineProvider>
        <ImageBadge status={status({ state: 'failed' })} onOpen={() => {}} />
      </MantineProvider>,
    )
    expect(screen.getByTestId('image-badge')).toHaveTextContent('failed')
  })

  // A container pool runs an image somebody else published, so there is
  // nothing here to report and nothing to open.
  it('has nothing to say about a container pool', () => {
    render(
      <MantineProvider>
        <ImageBadge status={status({ state: 'none', ready: true })} onOpen={() => {}} />
      </MantineProvider>,
    )
    expect(screen.queryByTestId('image-badge')).not.toBeInTheDocument()
  })

  it('opens the image when clicked', async () => {
    const onOpen = vi.fn()
    render(
      <MantineProvider>
        <ImageBadge status={status()} onOpen={onOpen} />
      </MantineProvider>,
    )
    await userEvent.click(screen.getByTestId('image-badge'))
    expect(onOpen).toHaveBeenCalled()
  })
})

describe('PoolImagePanel', () => {
  // The failure is the case this exists for: an empty pool with no reason was
  // what the fleet page used to show once the banner had been replaced.
  it('says why a pool has no runners, and that nothing will retry on its own', async () => {
    poolImage.mockResolvedValue({
      status: status({
        state: 'failed',
        summary: 'its image did not build',
        build: build({ phase: 'failed', error: 'the recipe exited 1' }),
      }),
      builds: [build({ phase: 'failed', error: 'the recipe exited 1' })],
    })
    showPanel()

    expect(
      await screen.findByText('This pool has no runners until its image builds'),
    ).toBeInTheDocument()
    expect(screen.getAllByText(/the recipe exited 1/).length).toBeGreaterThan(0)
    expect(screen.getByText(/Nothing will try again on its own/)).toBeInTheDocument()
  })

  // The history is what the banner threw away. A failure fixed on the third
  // attempt is only readable if the first two are still there.
  it('lists every attempt, newest first, with its log', async () => {
    poolImage.mockResolvedValue({
      status: status({ state: 'ready', ready: true, summary: 'its image is built' }),
      builds: [
        build({ id: 4, phase: 'succeeded', seconds: 372 }),
        build({ id: 3, phase: 'failed', error: 'the recipe exited 1', seconds: 61 }),
      ],
    })
    showPanel()

    const attempts = await screen.findAllByTestId('image-build')
    expect(attempts).toHaveLength(2)
    expect(within(attempts[0]).getByText('built')).toBeInTheDocument()
    expect(within(attempts[1]).getByText('failed')).toBeInTheDocument()

    // The newest is what is read by default; the one underneath opens on a
    // click, and it is the log of that build that is fetched.
    expect(imageBuildLog).toHaveBeenCalledWith(4)
    imageBuildLog.mockResolvedValue('the recipe could not find the toolchain\n')
    await userEvent.click(attempts[1])
    await waitFor(() => expect(imageBuildLog).toHaveBeenCalledWith(3))
    expect(await screen.findByTestId('build-log')).toHaveTextContent(
      'the recipe could not find the toolchain',
    )
  })

  // A build that failed is never retried on its own, so pressing this is the
  // only way back for a recipe somebody has since fixed by hand.
  it('asks for another build', async () => {
    poolImage.mockResolvedValue({
      status: status({ state: 'failed', build: build({ phase: 'failed' }) }),
      builds: [build({ phase: 'failed' })],
    })
    buildPoolImage.mockResolvedValue(build({ id: 9, phase: 'queued' }))
    showPanel()

    await userEvent.click(await screen.findByRole('button', { name: /Build now/ }))
    expect(buildPoolImage).toHaveBeenCalledWith(1)
  })

  // Nothing may start a second build of the same image: they would fight over
  // one working directory.
  it('cannot ask for a build while one is happening', async () => {
    poolImage.mockResolvedValue({
      status: status({ state: 'building', build: build() }),
      builds: [build()],
    })
    showPanel()

    expect(await screen.findByRole('button', { name: /Build now/ })).toBeDisabled()
  })

  // The log has to be readable while the build is running. One that only
  // appears after it fails is one nobody could have watched.
  it('shows the log of a build that is still going', async () => {
    poolImage.mockResolvedValue({
      status: status({ build: build() }),
      builds: [build()],
    })
    imageBuildLog.mockResolvedValue('Get:14 http://archive.ubuntu.com noble/main amd64 gcc\n')
    showPanel()

    expect(await screen.findByTestId('build-log')).toHaveTextContent('amd64 gcc')
  })

  it('says so when this host has never tried', async () => {
    poolImage.mockResolvedValue({
      status: status({ state: 'unbuilt', summary: 'its image has not been built yet' }),
      builds: [],
    })
    showPanel()

    expect(await screen.findByText(/never tried to build/)).toBeInTheDocument()
  })
})

describe('duration', () => {
  it('reads the way somebody would say it', () => {
    expect(duration(0)).toBe('0s')
    expect(duration(59)).toBe('59s')
    expect(duration(252)).toBe('4m 12s')
    expect(duration(3900)).toBe('1h 5m')
  })

  // Clocks disagree, and a machine whose clock stepped backwards would
  // otherwise put "-3s" on the page.
  it('does not show negative time', () => {
    expect(duration(-3)).toBe('0s')
  })
})
