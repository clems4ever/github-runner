import { useState } from 'react'
import {
  Alert,
  Anchor,
  Badge,
  Card,
  Center,
  Group,
  Loader,
  Modal,
  SimpleGrid,
  Stack,
  Table,
  Text,
  Title,
  Tooltip,
} from '@mantine/core'
import { IconAlertTriangle, IconServerOff } from '@tabler/icons-react'
import type { Credential, JobState, Pool, Runner, RunnerState, Scale } from '../api'
import { ActivityChart } from './ActivityChart'
import { PoolEditor } from './PoolEditor'

/**
 * The fleet as it actually is.
 *
 * Two columns say different things and both matter: STATE is what the host
 * reports about the machine, JOB is what GitHub reports about the runner
 * inside it. A machine can be up with nothing on it, and a runner can be busy
 * on a machine that is being drained.
 */
export function FleetPage({
  runners,
  pools,
  credentials,
  scaling,
  warnings,
  loading,
  onChange,
}: {
  runners: Runner[]
  pools: Pool[]
  credentials: Credential[]
  scaling: Record<string, Scale>
  warnings: string[]
  loading: boolean
  onChange: () => Promise<void>
}) {
  // The question a runner raises is usually about its pool — it is too small,
  // or it should not exist — so the answer is reachable from here rather than
  // through the pools page.
  const [editing, setEditing] = useState<Pool | null>(null)
  const busy = runners.filter((r) => r.job === 'busy').length
  const enabled = pools.filter((p) => p.enabled)
  const ceiling = enabled.reduce((total, p) => total + p.maxReplicas, 0)
  const stale = runners.filter((r) => !r.upToDate).length
  const elastic = enabled.filter((p) => p.maxReplicas > p.minReplicas)
  const poolsByName = new Map(pools.map((p) => [p.name, p]))

  if (loading) {
    return (
      <Center h={240}>
        <Loader />
      </Center>
    )
  }

  return (
    <Stack gap="lg">
      <Title order={3}>Fleet</Title>

      <SimpleGrid cols={{ base: 1, sm: 2, md: 4 }}>
        <Stat label="Runners" value={runners.length} hint={`up to ${ceiling}`} />
        <Stat label="Running a job" value={busy} />
        <Stat label="Idle" value={runners.filter((r) => r.job === 'idle').length} />
        <Stat
          label="Awaiting replacement"
          value={stale}
          hint={stale > 0 ? 'finishing their jobs first' : undefined}
        />
      </SimpleGrid>

      <ActivityChart pools={pools} />

      {elastic.length > 0 && (
        <Card withBorder padding="md">
          <Text size="xs" c="dimmed" tt="uppercase" fw={600} mb="xs">
            Scaling
          </Text>
          <Stack gap={6}>
            {elastic.map((pool) => {
              const decision = scaling[pool.name]
              const live = runners.filter((r) => r.pool === pool.name).length
              return (
                <Group key={pool.id} gap="xs" wrap="nowrap">
                  <Text size="sm" fw={500} w={140} truncate>
                    {pool.name}
                  </Text>
                  <Badge variant="light" size="sm" color={decision?.scaledUp ? 'blue' : 'gray'}>
                    {live} of {pool.minReplicas}–{pool.maxReplicas}
                  </Badge>
                  <Text size="sm" c="dimmed" truncate>
                    {decision?.reason ?? 'waiting for the first pass'}
                  </Text>
                </Group>
              )
            })}
          </Stack>
        </Card>
      )}

      {warnings.map((warning) => (
        <Alert key={warning} color="yellow" icon={<IconAlertTriangle size={18} />} variant="light">
          {warning}
        </Alert>
      ))}

      {runners.length === 0 ? (
        <Card withBorder padding="xl">
          <Center>
            <Stack align="center" gap="xs">
              <IconServerOff size={32} stroke={1.2} opacity={0.5} />
              <Text fw={500}>No runners yet</Text>
              <Text size="sm" c="dimmed">
                Add a credential, then a pool, and they will appear here within a few seconds.
              </Text>
            </Stack>
          </Center>
        </Card>
      ) : (
        <Card withBorder padding={0}>
          <Table highlightOnHover verticalSpacing="sm" horizontalSpacing="md">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Runner</Table.Th>
                <Table.Th>Pool</Table.Th>
                <Table.Th>Runtime</Table.Th>
                <Table.Th>State</Table.Th>
                <Table.Th>Job</Table.Th>
                <Table.Th>Configuration</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {runners.map((runner) => (
                <Table.Tr key={runner.name}>
                  <Table.Td>
                    <Text ff="monospace" size="sm">
                      {runner.name}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <PoolCell
                      name={runner.pool}
                      pool={poolsByName.get(runner.pool)}
                      onOpen={setEditing}
                    />
                  </Table.Td>
                  <Table.Td>
                    <Badge variant="default" size="sm">
                      {runner.runtime}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    <Group gap={6} wrap="nowrap">
                      <StateBadge state={runner.state} />
                      {runner.trouble && (
                        <Tooltip label={runner.trouble} multiline maw={420}>
                          <Badge
                            color="red"
                            variant="light"
                            leftSection={<IconAlertTriangle size={12} />}
                          >
                            failing
                          </Badge>
                        </Tooltip>
                      )}
                    </Group>
                  </Table.Td>
                  <Table.Td>
                    <JobBadge job={runner.job} />
                  </Table.Td>
                  <Table.Td>
                    {runner.upToDate ? (
                      <Text size="sm" c="dimmed">
                        up to date
                      </Text>
                    ) : (
                      <Tooltip label="It will be replaced once the job it is on finishes">
                        <Badge color="orange" variant="light">
                          superseded
                        </Badge>
                      </Tooltip>
                    )}
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Card>
      )}

      <Modal
        opened={editing !== null}
        onClose={() => setEditing(null)}
        title={editing ? `Edit ${editing.name}` : ''}
        size="lg"
      >
        {editing && (
          <PoolEditor
            pool={editing}
            credentials={credentials}
            onCancel={() => setEditing(null)}
            onSaved={async () => {
              setEditing(null)
              await onChange()
            }}
          />
        )}
      </Modal>
    </Stack>
  )
}

/**
 * A runner's pool, as a way into its definition.
 *
 * A runner can name a pool the daemon no longer has — it is being drained
 * after the pool was deleted — and there is nothing to open for that one, so
 * it stays plain text.
 */
function PoolCell({
  name,
  pool,
  onOpen,
}: {
  name: string
  pool?: Pool
  onOpen: (pool: Pool) => void
}) {
  if (!name) return <>—</>
  if (!pool) {
    return (
      <Text size="sm" c="dimmed">
        {name}
      </Text>
    )
  }
  return (
    <Anchor component="button" type="button" size="sm" onClick={() => onOpen(pool)}>
      {name}
    </Anchor>
  )
}

function Stat({ label, value, hint }: { label: string; value: number; hint?: string }) {
  return (
    <Card withBorder padding="md">
      <Text size="xs" c="dimmed" tt="uppercase" fw={600}>
        {label}
      </Text>
      <Group align="baseline" gap="xs">
        <Text fz={28} fw={600}>
          {value}
        </Text>
        {hint && (
          <Text size="xs" c="dimmed">
            {hint}
          </Text>
        )}
      </Group>
    </Card>
  )
}

const stateColours: Record<RunnerState, string> = {
  running: 'green',
  // Not a failure: a runner that is stopping is finishing the job it is on.
  stopping: 'yellow',
  stopped: 'gray',
}

function StateBadge({ state }: { state: RunnerState }) {
  return (
    <Badge color={stateColours[state] ?? 'gray'} variant="light">
      {state}
    </Badge>
  )
}

const jobColours: Record<JobState, string> = {
  busy: 'blue',
  idle: 'gray',
  // A runner GitHub cannot see is a problem; one it has never heard of is
  // merely not registered yet.
  offline: 'red',
  unknown: 'gray',
}

const jobHints: Record<JobState, string> = {
  busy: 'A job is running on it right now',
  idle: 'Registered and waiting for work',
  offline: 'Registered but not connected to GitHub',
  unknown: 'GitHub has no runner by this name yet',
}

function JobBadge({ job }: { job: JobState }) {
  return (
    <Tooltip label={jobHints[job] ?? ''}>
      <Badge color={jobColours[job] ?? 'gray'} variant={job === 'busy' ? 'filled' : 'light'}>
        {job}
      </Badge>
    </Tooltip>
  )
}
