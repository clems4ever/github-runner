import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MantineProvider } from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { PoolsPage } from './PoolsPage'
import { pretendNarrow } from '../test-setup'
import {
  api,
  type Credential,
  type Pool,
  type PoolImage,
  type Runner,
  type Scale,
} from '../api'

const CREDENTIALS: Credential[] = [
  { id: 1, name: 'fleet app', kind: 'app', appId: 42, hint: 'app 42', createdAt: '' },
]

const pool = (over: Partial<Pool> = {}): Pool => ({
  id: 1,
  name: 'web',
  scopeKind: 'repository',
  scope: 'clems4ever/github-runner',
  runtime: 'vm',
  nested: false,
  ephemeral: true,
  minReplicas: 1,
  maxReplicas: 4,
  labels: ['gpu'],
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

const runner = (name: string): Runner => ({
  name,
  pool: 'web',
  runtime: 'vm',
  state: 'running',
  job: 'idle',
  generation: 'abc123',
  upToDate: true,
})

function renderPage(
  pools: Pool[],
  runners: Runner[] = [],
  scaling: Record<string, Scale> = {},
  images: Record<string, PoolImage> = {},
) {
  const onChange = vi.fn().mockResolvedValue(undefined)
  return {
    ...render(
      <MantineProvider>
        <PoolsPage
          pools={pools}
          credentials={CREDENTIALS}
          runners={runners}
          scaling={scaling}
          images={images}
          onChange={onChange}
        />
      </MantineProvider>,
    ),
    onChange,
  }
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('PoolsPage', () => {
  // A machine pool takes no jobs until its image is built, so whether it is
  // built belongs on the row rather than in a banner over the fleet page.
  it('says where each pool image stands', () => {
    renderPage([pool()], [], {}, {
      web: {
        pool: 'web',
        image: 'runner-noble-default-abc123',
        state: 'failed',
        ready: false,
        summary: 'its image did not build',
      },
    })
    expect(screen.getByTestId('image-badge')).toHaveTextContent('failed')
  })

  it('lists the pools in a table on a wide screen', () => {
    renderPage([pool()])
    expect(screen.getByRole('table')).toBeInTheDocument()
    expect(screen.getByText('web')).toBeInTheDocument()
  })

  it('says what to do when there are no pools yet', () => {
    renderPage([])
    expect(screen.getByText('No pools yet')).toBeInTheDocument()
  })

  describe('on a phone', () => {
    let restore = () => {}
    afterEach(() => restore())

    it('draws a card per pool instead of a table', () => {
      restore = pretendNarrow()
      renderPage([pool()])

      expect(screen.queryByRole('table')).not.toBeInTheDocument()
      expect(screen.getByText('web')).toBeInTheDocument()
      expect(screen.getByText('clems4ever/github-runner')).toBeInTheDocument()
      expect(screen.getByText('2 vCPU · 4 GiB · 40 GiB')).toBeInTheDocument()
    })

    // The bug this page had: the menu that edits and deletes a pool lives in the
    // eighth column, which on a phone is past the right-hand edge of a table
    // that does not scroll. There was no way to edit a pool from a phone at all.
    it('keeps the actions menu within reach', async () => {
      restore = pretendNarrow()
      renderPage([pool()])

      await userEvent.click(screen.getByRole('button', { name: 'Actions for web' }))

      expect(await screen.findByText('Edit')).toBeInTheDocument()
      expect(screen.getByText('Delete')).toBeInTheDocument()
    })

    it('keeps the enabled switch, which is the other thing a row can do', () => {
      restore = pretendNarrow()
      renderPage([pool()])
      expect(screen.getByRole('switch', { name: 'Enable web' })).toBeChecked()
    })
  })
})

// A pool's size is the one thing an operator changes often, and until now it
// meant opening the editor. The rules the stepper follows matter as much as the
// buttons: a pool's kind never changes by accident, and shrinking never takes a
// job down with it.
describe('scaling a pool from the list', () => {
  it('grows a fixed pool by moving both bounds, so it stays fixed', async () => {
    const update = vi.spyOn(api, 'updatePool').mockResolvedValue(pool())
    const { onChange } = renderPage([pool({ minReplicas: 2, maxReplicas: 2 })])

    await userEvent.click(screen.getByRole('button', { name: 'Scale web up' }))

    expect(update).toHaveBeenCalledWith(
      1,
      expect.objectContaining({ minReplicas: 3, maxReplicas: 3 }),
    )
    expect(onChange).toHaveBeenCalled()
  })

  it('grows an autoscaling pool by raising its ceiling only', async () => {
    // The floor is the capacity someone promised; the ceiling is how far the
    // autoscaler may go, which is what "give it more room" means here.
    const update = vi.spyOn(api, 'updatePool').mockResolvedValue(pool())
    renderPage([pool({ minReplicas: 1, maxReplicas: 3 })])

    await userEvent.click(screen.getByRole('button', { name: 'Scale web up' }))

    expect(update).toHaveBeenCalledWith(
      1,
      expect.objectContaining({ minReplicas: 1, maxReplicas: 4 }),
    )
  })

  it('asks before shrinking, and says the runners are drained rather than killed', async () => {
    const update = vi.spyOn(api, 'updatePool').mockResolvedValue(pool())
    renderPage(
      [pool({ minReplicas: 3, maxReplicas: 3 })],
      [runner('web-1'), runner('web-2'), runner('web-3')],
    )

    await userEvent.click(screen.getByRole('button', { name: 'Scale web down' }))

    expect(update).not.toHaveBeenCalled()
    expect(screen.getByText(/finishes the job it is on/)).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Scale down' }))
    expect(update).toHaveBeenCalledWith(
      1,
      expect.objectContaining({ minReplicas: 2, maxReplicas: 2 }),
    )
  })

  it('changes nothing when the shrink is cancelled', async () => {
    const update = vi.spyOn(api, 'updatePool').mockResolvedValue(pool())
    renderPage([pool({ minReplicas: 2, maxReplicas: 2 })])

    await userEvent.click(screen.getByRole('button', { name: 'Scale web down' }))
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }))

    expect(update).not.toHaveBeenCalled()
  })

  it('refuses to take the last runner away', () => {
    // A pool with nothing running cannot accept a job, so it could never learn
    // that it needs to grow. Switching it off is the enabled switch's job.
    renderPage([pool({ minReplicas: 1, maxReplicas: 1 })])
    expect(screen.getByRole('button', { name: 'Scale web down' })).toBeDisabled()
  })

  it('refuses to shrink an autoscaling pool onto its floor', () => {
    // 1–2 stepped down is 1–1: a fixed pool. Switching the autoscaler off is a
    // decision for the editor, not a side effect of a minus button.
    renderPage([pool({ minReplicas: 1, maxReplicas: 2 })])
    expect(screen.getByRole('button', { name: 'Scale web down' })).toBeDisabled()
  })

  it('stops where the daemon would refuse the pool anyway', () => {
    renderPage([pool({ minReplicas: 1, maxReplicas: 64 })])
    expect(screen.getByRole('button', { name: 'Scale web up' })).toBeDisabled()
  })

  it('says so when the daemon refuses the change', async () => {
    vi.spyOn(api, 'updatePool').mockRejectedValue(new Error('no such credential'))
    const shown = vi.spyOn(notifications, 'show').mockImplementation(() => '')
    const { onChange } = renderPage([pool()])

    await userEvent.click(screen.getByRole('button', { name: 'Scale web up' }))

    expect(shown).toHaveBeenCalledWith(expect.objectContaining({ message: 'no such credential' }))
    // The row keeps saying what the daemon says, not what the click hoped for.
    expect(onChange).not.toHaveBeenCalled()
  })

  // The card carries the same cell as the row, so the phone gets this for free
  // — which is the point of there being one cell.
  it('is reachable from a phone', async () => {
    const restore = pretendNarrow()
    try {
      const update = vi.spyOn(api, 'updatePool').mockResolvedValue(pool())
      renderPage([pool({ minReplicas: 2, maxReplicas: 2 })])

      await userEvent.click(screen.getByRole('button', { name: 'Scale web up' }))

      expect(update).toHaveBeenCalledWith(
        1,
        expect.objectContaining({ minReplicas: 3, maxReplicas: 3 }),
      )
    } finally {
      restore()
    }
  })
})
