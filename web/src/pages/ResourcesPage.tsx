import {
  Alert,
  Badge,
  Card,
  Center,
  Group,
  Loader,
  Progress,
  SimpleGrid,
  Stack,
  Table,
  Text,
  Title,
  Tooltip,
} from '@mantine/core'
import { IconAlertTriangle } from '@tabler/icons-react'
import type { Commitment, HostResources, ResourceReport, RunnerUsage } from '../api'
import { HostChart } from './HostChart'

/**
 * What the host is actually using.
 *
 * The page answers two different questions and keeps them apart. The meters and
 * the table are measurements — what is being consumed right now. The
 * commitment row is arithmetic on the configuration: what the pools would take
 * if every one of them grew to its ceiling at the same moment. A fleet can sit
 * at four per cent and still be promising three times the machine it is on, and
 * the moment that matters is the one nobody is watching.
 */

/** Matches the chart's series colours, so a colour means one resource here. */
const meterColours = { cpu: 'blue', memory: 'orange', disk: 'teal' } as const

export function ResourcesPage({ report }: { report: ResourceReport | null }) {
  if (report === null || !report.ready || !report.host) {
    return (
      <Stack gap="lg">
        <Title order={3}>Resources</Title>
        <Card withBorder padding="xl">
          <Center>
            <Stack align="center" gap="xs">
              <Loader size="sm" />
              <Text size="sm" c="dimmed">
                Taking the first reading of the host.
              </Text>
            </Stack>
          </Center>
        </Card>
      </Stack>
    )
  }

  const host = report.host
  const runners = report.runners ?? []

  return (
    <Stack gap="lg">
      <Group justify="space-between" align="baseline">
        <Title order={3}>Resources</Title>
        {report.at && (
          <Text size="xs" c="dimmed">
            measured {new Date(report.at).toLocaleTimeString()}
          </Text>
        )}
      </Group>

      <SimpleGrid cols={{ base: 1, sm: 3 }}>
        <Meter
          label="CPU"
          colour={meterColours.cpu}
          percent={host.cpuPercent}
          detail={`${host.cpus} core${host.cpus === 1 ? '' : 's'}`}
        />
        <Meter
          label="Memory"
          colour={meterColours.memory}
          percent={percentOf(host.memoryUsedBytes, host.memoryTotalBytes)}
          detail={`${bytes(host.memoryUsedBytes)} of ${bytes(host.memoryTotalBytes)}`}
        />
        <Meter
          label="Disk"
          colour={meterColours.disk}
          percent={percentOf(host.diskUsedBytes, host.diskTotalBytes)}
          detail={`${bytes(host.diskUsedBytes)} of ${bytes(host.diskTotalBytes)}`}
          hint={host.diskPath}
        />
      </SimpleGrid>

      <HostChart />

      {report.committed && <Committed committed={report.committed} host={host} />}

      {(report.warnings ?? []).map((warning) => (
        <Alert key={warning} color="yellow" icon={<IconAlertTriangle size={18} />} variant="light">
          {warning}
        </Alert>
      ))}

      <Card withBorder padding={0}>
        <Table highlightOnHover verticalSpacing="sm" horizontalSpacing="md">
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Runner</Table.Th>
              <Table.Th>Pool</Table.Th>
              <Table.Th>Runtime</Table.Th>
              <Table.Th>CPU</Table.Th>
              <Table.Th>Memory</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {runners.length === 0 ? (
              <Table.Tr>
                <Table.Td colSpan={5}>
                  <Text size="sm" c="dimmed" ta="center" py="md">
                    Nothing is running on this host yet.
                  </Text>
                </Table.Td>
              </Table.Tr>
            ) : (
              runners.map((runner) => <RunnerRow key={runner.name} runner={runner} />)
            )}
          </Table.Tbody>
        </Table>
      </Card>
    </Stack>
  )
}

function RunnerRow({ runner }: { runner: RunnerUsage }) {
  return (
    <Table.Tr>
      <Table.Td>
        <Text ff="monospace" size="sm">
          {runner.name}
        </Text>
      </Table.Td>
      <Table.Td>{runner.pool || '—'}</Table.Td>
      <Table.Td>
        <Badge variant="default" size="sm">
          {runner.runtime}
        </Badge>
      </Table.Td>
      <Table.Td>
        {runner.cpuPercent === null || runner.cpuPercent === undefined ? (
          // A rate needs two readings and this runner has one. A dash is the
          // truth; a zero would be a machine mid-boot shown as doing nothing.
          <Tooltip label="Measured on the next reading">
            <Text size="sm" c="dimmed">
              —
            </Text>
          </Tooltip>
        ) : (
          <Text size="sm">{runner.cpuPercent.toFixed(1)}%</Text>
        )}
      </Table.Td>
      <Table.Td>
        <Text size="sm">{bytes(runner.memoryBytes)}</Text>
      </Table.Td>
    </Table.Tr>
  )
}

/**
 * What the pools have promised, against what the host has.
 *
 * Oversubscription is not flagged as an error, because it is often deliberate:
 * pools rarely all peak together, and a host that could never be oversubscribed
 * would be a host running well below what it could. It is stated plainly and
 * left to the operator, which is the difference between a fact and a nag.
 */
function Committed({ committed, host }: { committed: Commitment; host: HostResources }) {
  const overCPU = committed.cpus > host.cpus
  const overMemory = committed.memoryBytes > host.memoryTotalBytes

  return (
    <Card withBorder padding="md">
      <Text size="xs" c="dimmed" tt="uppercase" fw={600} mb="xs">
        Committed at full stretch
      </Text>
      <Group gap="xl">
        <Fact label="Runners" value={String(committed.runners)} />
        <Fact
          label="CPU"
          value={`${committed.cpus} of ${host.cpus}`}
          flagged={overCPU}
          note={overCPU ? 'more than the host has' : undefined}
        />
        <Fact
          label="Memory"
          value={`${bytes(committed.memoryBytes)} of ${bytes(host.memoryTotalBytes)}`}
          flagged={overMemory}
          note={overMemory ? 'more than the host has' : undefined}
        />
        <Fact label="Disk" value={bytes(committed.diskBytes)} note="machines only" />
      </Group>
    </Card>
  )
}

function Fact({
  label,
  value,
  note,
  flagged,
}: {
  label: string
  value: string
  note?: string
  flagged?: boolean
}) {
  return (
    <div>
      <Text size="xs" c="dimmed">
        {label}
      </Text>
      <Group gap={6} align="baseline">
        <Text size="sm" fw={500}>
          {value}
        </Text>
        {note && (
          // Flagged states carry the word as well as the colour: a reader who
          // cannot tell the two apart still reads "more than the host has".
          <Text size="xs" c={flagged ? 'orange' : 'dimmed'}>
            {note}
          </Text>
        )}
      </Group>
    </div>
  )
}

function Meter({
  label,
  percent,
  detail,
  hint,
  colour,
}: {
  label: string
  percent: number
  detail: string
  hint?: string
  colour: string
}) {
  return (
    <Card withBorder padding="md">
      <Group justify="space-between" align="baseline">
        <Text size="xs" c="dimmed" tt="uppercase" fw={600}>
          {label}
        </Text>
        {hint && (
          <Text size="xs" c="dimmed" ff="monospace" truncate maw={160}>
            {hint}
          </Text>
        )}
      </Group>
      {/* The number is always written out, never left to the bar alone. */}
      <Text fz={28} fw={600}>
        {percent.toFixed(percent >= 10 ? 0 : 1)}%
      </Text>
      <Progress
        value={percent}
        color={colour}
        size="sm"
        mt={4}
        aria-label={`${label}: ${percent.toFixed(0)} per cent`}
      />
      <Text size="xs" c="dimmed" mt={6}>
        {detail}
      </Text>
    </Card>
  )
}

function percentOf(used: number, total: number): number {
  if (!total || total <= 0) return 0
  return (used / total) * 100
}

/**
 * Bytes, in the units people say out loud.
 *
 * Powers of 1024 labelled GB rather than GiB, because that is what every other
 * number on this host is: a pool asking for 4096 MB gets 4096 MiB, and the
 * meters have to agree with the pool editor.
 */
export function bytes(value: number): string {
  if (!value || value <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  let size = value
  let unit = 0
  while (size >= 1024 && unit < units.length - 1) {
    size /= 1024
    unit++
  }
  return `${size.toFixed(size >= 100 || unit === 0 ? 0 : 1)} ${units[unit]}`
}
