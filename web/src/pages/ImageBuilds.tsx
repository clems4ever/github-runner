import { Alert, Badge, Card, Code, Group, Loader, Stack, Text } from '@mantine/core'
import { IconAlertTriangle, IconCircleCheck } from '@tabler/icons-react'
import type { ImageBuild } from '../api'

/**
 * What the host is building, and what last happened to each pool's image.
 *
 * A machine pool with no runners is either starting one or building the image
 * it would boot, and from the fleet table those are the same picture: a pool
 * short of its minimum, with nothing to say why. The build takes minutes, and
 * when it fails it takes them again on the next pass — so a pool whose recipe
 * does not work looks like a pool that is merely slow, for ever.
 *
 * This is the difference, and it is deliberately at the top of the page rather
 * than inside a pool's own card: it is the answer to a question somebody is
 * asking right now.
 */
export function ImageBuilds({ builds }: { builds: ImageBuild[] }) {
  if (builds.length === 0) return null
  return (
    <Stack gap="sm">
      {builds.map((build) =>
        build.phase === 'failed' ? (
          <FailedBuild key={build.pool} build={build} />
        ) : build.phase === 'done' ? (
          <FinishedBuild key={build.pool} build={build} />
        ) : (
          <RunningBuild key={build.pool} build={build} />
        ),
      )}
    </Stack>
  )
}

function RunningBuild({ build }: { build: ImageBuild }) {
  return (
    <Card withBorder padding="md" data-testid="image-build">
      <Group gap="sm" wrap="nowrap" align="flex-start">
        <Loader size="sm" mt={2} />
        <Stack gap={4} style={{ flex: 1, minWidth: 0 }}>
          <Group gap="xs" wrap="wrap">
            <Text fw={500}>Building the golden image for {build.pool}</Text>
            <Badge variant="light" size="sm">
              {duration(build.seconds)}
            </Badge>
            {/* Not "stuck": the daemon cannot know that. It knows the build
                has stopped saying anything, which is what it says. */}
            {build.silent && (
              <Badge color="yellow" variant="light" size="sm">
                nothing printed for a while
              </Badge>
            )}
          </Group>
          {/* The build's own words. This is the whole point of the panel: a
              progress display that says what is happening beats one that says
              only that something is. */}
          {build.detail && (
            <Text size="sm" c="dimmed" style={{ wordBreak: 'break-word' }}>
              {build.detail}
            </Text>
          )}
          <Text size="xs" c="dimmed">
            {build.phase === 'fetching'
              ? 'Fetching the image every build starts from. This happens once per host.'
              : `${build.runner} is doing the build; anything else waiting on this image waits for it. ` +
                'A machine appears when it finishes.'}
          </Text>
        </Stack>
      </Group>
    </Card>
  )
}

function FailedBuild({ build }: { build: ImageBuild }) {
  return (
    <Alert
      color="red"
      variant="light"
      icon={<IconAlertTriangle size={18} />}
      title={`The golden image for ${build.pool} did not build`}
      data-testid="image-build"
    >
      <Stack gap={6}>
        <Text size="sm" style={{ wordBreak: 'break-word' }}>
          {build.error}
        </Text>
        {/* Where to read the rest of it: a failed build's console is the only
            account of what a recipe did, and it is kept on purpose.
            Suppressed when the error already names it, which most of them do —
            the same message goes to the journal, where there is no field to
            put a path in. Printing it twice reads like two different files. */}
        {build.console && !build.error?.includes(build.console) && (
          <Text size="xs" c="dimmed">
            The console is at <Code>{build.console}</Code>
          </Text>
        )}
        <Text size="xs" c="dimmed">
          Failed after {duration(build.seconds)}. The pool keeps running the image it already had,
          and the next pass will try again — so a recipe that cannot work will fail here every time
          until it is changed.
        </Text>
      </Stack>
    </Alert>
  )
}

/**
 * A build that worked, shown only for the hour after it finished: worth seeing
 * while whoever changed the recipe is still watching, and noise tomorrow.
 */
function FinishedBuild({ build }: { build: ImageBuild }) {
  return (
    <Alert
      color="green"
      variant="light"
      icon={<IconCircleCheck size={18} />}
      data-testid="image-build"
    >
      <Text size="sm">
        Built the golden image for {build.pool} in {duration(build.seconds)}. Its runners are being
        replaced as they finish their jobs.
      </Text>
    </Alert>
  )
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
