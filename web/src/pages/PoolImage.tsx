import { useCallback, useEffect, useRef, useState } from 'react'
import {
  Alert,
  Badge,
  Button,
  Center,
  Group,
  Loader,
  Modal,
  ScrollArea,
  Stack,
  Text,
  Tooltip,
  UnstyledButton,
} from '@mantine/core'
import { notifications } from '@mantine/notifications'
import {
  IconAlertTriangle,
  IconCircleCheck,
  IconClock,
  IconHammer,
  IconRefresh,
} from '@tabler/icons-react'
import { api, type ImageBuild, type ImageState, type Pool, type PoolImage } from '../api'
import { useNarrow } from '../responsive'

/**
 * A pool's golden image: whether it is built, and everything this host has
 * tried.
 *
 * It lives against the pool rather than in a banner over the fleet, which is
 * where it used to be. A banner said one thing about one build, replaced it
 * with the next, and threw the log away — so the failure that explained why a
 * pool was empty all morning was gone by the time anybody asked. The question
 * "can this pool run a job" is a question about the pool, and this is where it
 * is answered.
 */

/** How the state reads on a badge, and what colour it deserves. */
const states: Record<ImageState, { label: string; colour: string; hint: string }> = {
  ready: {
    label: 'built',
    colour: 'green',
    hint: 'Its image is built, so this pool can take jobs',
  },
  building: {
    label: 'building',
    colour: 'blue',
    hint: 'Its image is being built. The pool gets no runners until it is done',
  },
  queued: {
    label: 'queued',
    colour: 'blue',
    hint: 'Its image is waiting to be built; one image is built at a time',
  },
  failed: {
    label: 'failed',
    colour: 'red',
    hint: 'Its image did not build, so this pool has no runners. Open it to read the log',
  },
  unbuilt: {
    label: 'not built',
    colour: 'yellow',
    hint: 'Its image has not been built on this host yet',
  },
  none: { label: '—', colour: 'gray', hint: 'A container pool runs an image somebody else published' },
}

/**
 * The signal on the pool's row: one word for whether it can work, and a way
 * into why.
 */
export function ImageBadge({ status, onOpen }: { status?: PoolImage; onOpen: () => void }) {
  if (!status || status.state === 'none') {
    return (
      <Text size="sm" c="dimmed">
        —
      </Text>
    )
  }
  const { label, colour, hint } = states[status.state] ?? states.unbuilt
  const building = status.state === 'building' || status.state === 'queued'
  return (
    <Tooltip label={status.summary || hint} multiline maw={360}>
      <UnstyledButton onClick={onOpen} aria-label={`Image for ${status.pool}`}>
        <Badge
          color={colour}
          // Filled for the one state that is a problem, so a table of pools
          // reads as quiet until something is wrong with one of them.
          variant={status.state === 'failed' ? 'filled' : 'light'}
          size="sm"
          leftSection={building ? <Loader size={10} color={colour} /> : undefined}
          style={{ cursor: 'pointer', whiteSpace: 'nowrap' }}
          // A badge ellipsises its own label by default, which turned the one
          // word this column exists to say into "BUILDIN…". It is three
          // words at most; it can have the room.
          styles={{ label: { overflow: 'visible' } }}
          data-testid="image-badge"
        >
          {building && status.build ? `${label} ${duration(status.build.seconds)}` : label}
        </Badge>
      </UnstyledButton>
    </Tooltip>
  )
}

/**
 * Everything about one pool's image, in a panel of its own: what it is doing
 * now, the log it is writing, and every attempt before this one.
 */
export function PoolImagePanel({
  pool,
  opened,
  onClose,
}: {
  pool: Pool | null
  opened: boolean
  onClose: () => void
}) {
  const narrow = useNarrow()
  const [status, setStatus] = useState<PoolImage | null>(null)
  const [builds, setBuilds] = useState<ImageBuild[]>([])
  const [selected, setSelected] = useState<number | null>(null)
  const [log, setLog] = useState('')
  const [loading, setLoading] = useState(true)
  const [asking, setAsking] = useState(false)
  // Which build the log on screen belongs to, so switching to another one does
  // not show the previous log until the fetch lands.
  const showing = useRef<number | null>(null)

  const refresh = useCallback(async () => {
    if (!pool) return
    try {
      const answer = await api.poolImage(pool.id)
      setStatus(answer.status)
      setBuilds(answer.builds)
      // Follow the newest build until somebody picks another one to read.
      setSelected((current) => current ?? answer.builds[0]?.id ?? null)
    } catch (error) {
      notifications.show({
        color: 'red',
        title: 'Could not read the image',
        message: error instanceof Error ? error.message : String(error),
      })
    } finally {
      setLoading(false)
    }
  }, [pool])

  useEffect(() => {
    if (!opened) return
    setLoading(true)
    setSelected(null)
    setLog('')
    showing.current = null
    void refresh()
  }, [opened, refresh])

  const build = builds.find((b) => b.id === selected) ?? null
  const live = build !== null && unfinished(build)

  // The log of the build being read, refreshed while that build is still
  // running: a log somebody can only see once the build is over is a log they
  // could not have watched.
  useEffect(() => {
    if (!opened || selected === null) return
    let cancelled = false
    const read = async () => {
      try {
        const text = await api.imageBuildLog(selected)
        if (!cancelled) {
          showing.current = selected
          setLog(text)
        }
      } catch {
        /* the panel already says what state the build is in */
      }
    }
    void read()
    if (!live) return
    const timer = setInterval(() => {
      void read()
      void refresh()
    }, 3000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [opened, selected, live, refresh])

  const askForABuild = async () => {
    if (!pool) return
    setAsking(true)
    try {
      const started = await api.buildPoolImage(pool.id)
      setSelected(started.id)
      setLog('')
      await refresh()
    } catch (error) {
      notifications.show({
        color: 'red',
        title: 'Could not start the build',
        message: error instanceof Error ? error.message : String(error),
      })
    } finally {
      setAsking(false)
    }
  }

  return (
    <Modal
      opened={opened}
      onClose={onClose}
      title={pool ? `Image for ${pool.name}` : ''}
      size="xl"
      fullScreen={narrow}
    >
      {loading ? (
        <Center h={200}>
          <Loader />
        </Center>
      ) : (
        <Stack gap="md">
          <Group justify="space-between" align="flex-start" gap="sm" wrap="wrap">
            <Stack gap={4} style={{ flex: 1, minWidth: 220 }}>
              <Group gap="xs">
                <ImageBadge status={status ?? undefined} onOpen={() => {}} />
                {/* The alert below says all of this and more when a build has
                    failed, and saying it twice reads like two problems. */}
                {status?.state !== 'failed' && <Text size="sm">{status?.summary}</Text>}
              </Group>
              {status?.image && (
                <Text size="xs" c="dimmed" ff="monospace" style={{ wordBreak: 'break-all' }}>
                  {status.image}
                </Text>
              )}
            </Stack>
            <Tooltip
              label={
                status?.state === 'building' || status?.state === 'queued'
                  ? 'It is already building'
                  : 'Build this image again. Nothing else will: a build that failed is never retried on its own'
              }
            >
              <Button
                leftSection={
                  status?.state === 'ready' ? <IconRefresh size={16} /> : <IconHammer size={16} />
                }
                loading={asking}
                disabled={status?.state === 'building' || status?.state === 'queued'}
                onClick={askForABuild}
              >
                {status?.state === 'ready' ? 'Build again' : 'Build now'}
              </Button>
            </Tooltip>
          </Group>

          {/* Why the pool is empty, said once, where the pool is. */}
          {status && !status.ready && status.state === 'failed' && (
            <Alert
              color="red"
              variant="light"
              icon={<IconAlertTriangle size={18} />}
              title="This pool has no runners until its image builds"
            >
              <Text size="sm" style={{ wordBreak: 'break-word' }}>
                {status.build?.error}
              </Text>
              <Text size="xs" c="dimmed" mt={6}>
                Nothing will try again on its own. Fix the recipe — saving it starts a build of the
                new image — or press Build now to try this one again.
              </Text>
            </Alert>
          )}

          {builds.length === 0 ? (
            <Text size="sm" c="dimmed">
              This host has never tried to build this pool's image.
            </Text>
          ) : (
            <Stack gap={6}>
              <Text size="xs" c="dimmed" tt="uppercase" fw={600}>
                Attempts
              </Text>
              {builds.map((attempt) => (
                <BuildRow
                  key={attempt.id}
                  build={attempt}
                  selected={attempt.id === selected}
                  onSelect={() => {
                    setSelected(attempt.id)
                    setLog('')
                  }}
                />
              ))}
            </Stack>
          )}

          {build && (
            <Stack gap={6}>
              <Group gap="xs">
                <Text size="xs" c="dimmed" tt="uppercase" fw={600}>
                  Log
                </Text>
                {live && <Loader size={12} />}
                {build.silent && (
                  <Badge color="yellow" variant="light" size="xs">
                    nothing printed for a while
                  </Badge>
                )}
              </Group>
              <Console text={showing.current === build.id ? log : ''} follow={live} />
            </Stack>
          )}
        </Stack>
      )}
    </Modal>
  )
}

/** One attempt, as a line: what it did, when, and how long it took. */
function BuildRow({
  build,
  selected,
  onSelect,
}: {
  build: ImageBuild
  selected: boolean
  onSelect: () => void
}) {
  const failed = build.phase === 'failed'
  const done = build.phase === 'succeeded'
  return (
    <UnstyledButton onClick={onSelect} data-testid="image-build">
      <Group
        gap="xs"
        wrap="nowrap"
        p={6}
        style={{
          borderRadius: 6,
          border: '1px solid var(--mantine-color-default-border)',
          background: selected ? 'var(--mantine-color-default-hover)' : undefined,
        }}
      >
        {failed ? (
          <IconAlertTriangle size={16} color="var(--mantine-color-red-6)" />
        ) : done ? (
          <IconCircleCheck size={16} color="var(--mantine-color-green-6)" />
        ) : (
          <IconClock size={16} />
        )}
        <Text size="sm" fw={500} style={{ flexShrink: 0 }}>
          {phaseLabel(build)}
        </Text>
        <Text size="xs" c="dimmed" style={{ flexShrink: 0 }}>
          {duration(build.seconds)}
        </Text>
        <Text size="xs" c="dimmed" truncate style={{ flex: 1 }}>
          {build.error || build.detail || when(build.startedAt)}
        </Text>
        {build.trigger === 'requested' && (
          <Badge size="xs" variant="default" style={{ flexShrink: 0 }}>
            asked for
          </Badge>
        )}
      </Group>
    </UnstyledButton>
  )
}

/**
 * The build's own words.
 *
 * It scrolls itself to the bottom while a build is running, because the line
 * being waited for is always the last one — and stops doing that once it has
 * finished, so a log being read does not jump.
 */
function Console({ text, follow }: { text: string; follow: boolean }) {
  const viewport = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (follow && viewport.current) {
      viewport.current.scrollTop = viewport.current.scrollHeight
    }
  }, [text, follow])

  return (
    <ScrollArea.Autosize mah={360} viewportRef={viewport} type="auto">
      <Text
        component="pre"
        ff="monospace"
        size="xs"
        p="xs"
        data-testid="build-log"
        style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word', margin: 0 }}
      >
        {text || 'Nothing has been written yet.'}
      </Text>
    </ScrollArea.Autosize>
  )
}

function phaseLabel(build: ImageBuild): string {
  switch (build.phase) {
    case 'succeeded':
      return 'built'
    case 'failed':
      return 'failed'
    case 'fetching':
      return 'fetching Ubuntu'
    case 'running':
      return 'provisioning'
    default:
      return 'queued'
  }
}

function unfinished(build: ImageBuild): boolean {
  return build.phase === 'queued' || build.phase === 'fetching' || build.phase === 'running'
}

/** When something happened, in the reader's own timezone. */
function when(at: string): string {
  const date = new Date(at)
  return Number.isNaN(date.getTime()) ? '' : date.toLocaleString()
}

/**
 * Seconds as a person would say them. A build is minutes, so "4m 12s" is the
 * common case and the hour is there for the one that went wrong.
 */
export function duration(seconds: number): string {
  if (seconds < 60) return `${Math.max(seconds, 0)}s`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes}m ${seconds % 60}s`
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m`
}
