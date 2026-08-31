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
import { Field, useNarrow } from '../responsive'
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
  const narrow = useNarrow()
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

      {/* Two across on a phone rather than one: four full-width cards is a
          screenful of numbers before the fleet itself comes into view. */}
      <SimpleGrid cols={{ base: 2, md: 4 }} spacing={{ base: 'xs', sm: 'md' }}>
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
              // The reason is the point of this panel, so on a narrow screen it
              // gets its own line rather than being truncated to make room.
              return (
                <Group key={pool.id} gap="xs" wrap={narrow ? 'wrap' : 'nowrap'}>
                  <Text size="sm" fw={500} w={narrow ? undefined : 140} truncate={!narrow}>
                    {pool.name}
                  </Text>
                  <Badge variant="light" size="sm" color={decision?.scaledUp ? 'blue' : 'gray'}>
                    {live} of {pool.minReplicas}–{pool.maxReplicas}
                  </Badge>
                  <Text size="sm" c="dimmed" truncate={!narrow} style={narrow ? { width: '100%' } : undefined}>
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
      ) : narrow ? (
        <Stack gap="sm">
          {runners.map((runner) => (
            <RunnerCard
              key={runner.name}
              runner={runner}
              pool={poolsByName.get(runner.pool)}
              onOpen={setEditing}
            />
          ))}
        </Stack>
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
                    <StateCell runner={runner} />
                  </Table.Td>
                  <Table.Td>
                    <JobBadge job={runner.job} />
                  </Table.Td>
                  <Table.Td>
                    <ConfigurationCell runner={runner} />
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
        // As on the pools page: the editor is a long form, and on a phone it
        // gets the screen rather than a panel floating inside one.
        fullScreen={narrow}
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

/**
 * One runner, as a card.
 *
 * The same six facts the table carries, in the same order: what a phone loses
 * against a desktop is the ability to compare two runners at a glance, not any
 * of what either one says. The pool is the same way into its definition it is
 * in the table — that route should not be the thing a phone loses.
 */
function RunnerCard({
  runner,
  pool,
  onOpen,
}: {
  runner: Runner
  pool?: Pool
  onOpen: (pool: Pool) => void
}) {
  return (
    <Card withBorder padding="md">
      <Stack gap="xs">
        <Group justify="space-between" gap="xs" wrap="nowrap" align="flex-start">
          <Text ff="monospace" size="sm" fw={500} style={{ wordBreak: 'break-all' }}>
            {runner.name}
          </Text>
          <Badge variant="default" size="sm" style={{ flexShrink: 0 }}>
            {runner.runtime}
          </Badge>
        </Group>
        <Field label="Pool">
          <PoolCell name={runner.pool} pool={pool} onOpen={onOpen} />
        </Field>
        <Field label="State">
          <StateCell runner={runner} />
        </Field>
        <Field label="Job">
          <JobBadge job={runner.job} />
        </Field>
        <Field label="Configuration">
          <ConfigurationCell runner={runner} />
        </Field>
      </Stack>
    </Card>
  )
}

function StateCell({ runner }: { runner: Runner }) {
  return (
    <Group gap={6} wrap="wrap" justify="flex-end">
      <StateBadge state={runner.state} />
      {runner.trouble && (
        <Tooltip label={runner.trouble} multiline maw={420}>
          <Badge color="red" variant="light" leftSection={<IconAlertTriangle size={12} />}>
            failing
          </Badge>
        </Tooltip>
      )}
    </Group>
  )
}

function ConfigurationCell({ runner }: { runner: Runner }) {
  if (runner.upToDate) {
    return (
      <Text size="sm" c="dimmed">
        up to date
      </Text>
    )
  }
  return (
    <Tooltip label="It will be replaced once the job it is on finishes">
      <Badge color="orange" variant="light">
        superseded
      </Badge>
    </Tooltip>
  )
}

function Stat({ label, value, hint }: { label: string; value: number; hint?: string }) {
  return (
    <Card withBorder p={{ base: 'sm', sm: 'md' }}>
      <Text size="xs" c="dimmed" tt="uppercase" fw={600}>
        {label}
      </Text>
      <Group align="baseline" gap="xs">
        <Text fz={{ base: 24, sm: 28 }} fw={600}>
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
  // On its way up. An ephemeral runner is in this state after every job — it
  // deregisters itself the moment the job ends and a fresh machine boots — so
  // for a busy pool it is most of what anybody sees.
  starting: 'cyan',
  // A runner GitHub cannot see is a problem; one it has never heard of is
  // merely not registered yet.
  offline: 'red',
  unknown: 'gray',
}

const jobHints: Record<JobState, string> = {
  busy: 'A job is running on it right now',
  idle: 'Registered and waiting for work',
  starting: 'Booting. It registers with GitHub when it is ready, which takes a minute or two',
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
