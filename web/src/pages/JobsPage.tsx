import { useEffect, useMemo, useState } from 'react'
import {
  Alert,
  Card,
  Center,
  Group,
  Loader,
  SegmentedControl,
  Select,
  Stack,
  Table,
  Text,
  Title,
  useComputedColorScheme,
} from '@mantine/core'
import { BarChart } from '@mantine/charts'
import { IconInfoCircle } from '@tabler/icons-react'
import { api, type JobDay, type Pool, type PoolJobs } from '../api'
import { Field, useNarrow } from '../responsive'

/**
 * What each pool has actually run, so a pool can be sized against its work
 * rather than against a hunch.
 *
 * Two numbers per pool, and they answer different questions. The count says how
 * often the pool is asked for something; the time says how much of the pool
 * that took — runner-time, so two runners busy for a minute is two minutes,
 * which is the figure a pool would have had to be bigger to absorb.
 *
 * Neither is exact, and the page says so rather than implying a precision the
 * daemon does not have. It never sees a job: it asks GitHub what each runner is
 * doing once a reconcile pass, so a job that starts and finishes between two
 * passes leaves no trace, and the time is a sum of intervals rather than a
 * stopwatch. Over a month of passes that is close enough to decide a pool is
 * too small, and nowhere near exact enough to bill anyone for.
 */

/** The bar's colour, from the validated categorical palette (slot 1). */
const colours = {
  light: { bar: '#2a78d6', grid: '#e1e0d9', muted: '#898781' },
  dark: { bar: '#3987e5', grid: '#2c2c2a', muted: '#898781' },
}

const ranges = [
  { value: '7', label: '7d' },
  { value: '30', label: '30d' },
  { value: '90', label: '90d' },
]

export function JobsPage({ pools }: { pools: Pool[] }) {
  const scheme = useComputedColorScheme('light')
  const palette = colours[scheme]
  const narrow = useNarrow()

  const [days, setDays] = useState('30')
  // Empty is the whole fleet. Narrowing to one pool is how a single pool's
  // trend is read, which is the question a resize actually turns on.
  const [pool, setPool] = useState<string | null>(null)
  const [history, setHistory] = useState<{ pools: PoolJobs[]; days: JobDay[] } | null>(null)
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    let cancelled = false
    const load = async () => {
      try {
        const tally = await api.jobs(Number(days))
        if (!cancelled) {
          setHistory({ pools: tally.pools ?? [], days: tally.days ?? [] })
          setFailed(false)
        }
      } catch {
        if (!cancelled) setFailed(true)
      }
    }
    void load()
    // Far slower than the fleet table: this is a tally per day, and today's
    // row moves by a reconcile pass at a time.
    const timer = setInterval(() => void load(), 60_000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [days])

  const selected = useMemo(
    () => (history?.days ?? []).filter((entry) => !pool || entry.pool === pool),
    [history, pool],
  )
  const rows = useMemo(
    () => poolRows(history?.pools ?? [], history?.days ?? [], pools, pool),
    [history, pools, pool],
  )
  const total = useMemo(
    () =>
      rows.reduce(
        (sum, row) => ({ jobs: sum.jobs + row.jobs, seconds: sum.seconds + row.seconds }),
        { jobs: 0, seconds: 0 },
      ),
    [rows],
  )

  const data = useMemo(() => {
    const byDay = new Map<string, number>()
    for (const entry of selected) {
      byDay.set(entry.day, (byDay.get(entry.day) ?? 0) + entry.seconds)
    }
    return [...byDay.entries()]
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([day, seconds]) => ({ day, 'Runner-hours': Number((seconds / 3600).toFixed(2)) }))
  }, [selected])

  return (
    <Stack gap="lg">
      <Group justify="space-between" align="baseline" gap="sm" wrap="wrap">
        <Title order={3}>Jobs</Title>
        {/* On their own line once the heading and the filters no longer share
            one, rather than each shrinking to something unreadable. */}
        <Group gap="xs" wrap="nowrap" style={narrow ? { flex: '1 1 100%' } : undefined}>
          {pools.length > 1 && (
            <Select
              size="xs"
              w={narrow ? undefined : 180}
              flex={narrow ? 1 : undefined}
              aria-label="Pool"
              placeholder="All pools"
              clearable
              value={pool}
              onChange={setPool}
              data={pools.map((p) => ({ value: p.name, label: p.name }))}
            />
          )}
          <SegmentedControl size="xs" data={ranges} value={days} onChange={setDays} />
        </Group>
      </Group>

      {/* Said once, at the top, rather than hedged next to every number. */}
      <Alert variant="light" color="gray" icon={<IconInfoCircle size={18} />}>
        Counted by watching runners, not by asking GitHub for its own record of each job. The daemon
        looks once a reconcile pass, so a job shorter than one pass is not seen and the time is a sum
        of intervals. Good enough to size a pool from; not an invoice.
      </Alert>

      <Card withBorder p={{ base: 'sm', sm: 'md' }}>
        {/* Three facts in a row is two words each on a phone; they wrap. */}
        <Group gap="xl" wrap="wrap">
          <Fact label="Jobs" value={String(total.jobs)} />
          <Fact label="Time on jobs" value={duration(total.seconds)} hint="runner-time" />
          <Fact
            label="Mean job"
            value={total.jobs > 0 ? duration(total.seconds / total.jobs) : '—'}
          />
        </Group>
      </Card>

      <Card withBorder p={{ base: 'sm', sm: 'md' }}>
        <Group justify="space-between" mb="sm">
          <div>
            <Text fw={500}>Runner-hours a day</Text>
            <Text size="xs" c="dimmed">
              {pool ? `${pool} only` : 'Every pool on this host, added together'}
            </Text>
          </div>
        </Group>

        {failed ? (
          <Center h={220}>
            <Stack align="center" gap={4}>
              <Text size="sm" fw={500}>
                Could not read the tally.
              </Text>
              <Text size="xs" c="dimmed">
                The fleet itself is unaffected — this is only a record of what it has run.
              </Text>
            </Stack>
          </Center>
        ) : history === null ? (
          <Center h={220}>
            <Loader size="sm" />
          </Center>
        ) : data.length === 0 ? (
          <Center h={220}>
            <Stack align="center" gap={4}>
              <Text size="sm" fw={500}>
                Nothing run yet
              </Text>
              <Text size="xs" c="dimmed">
                {pool
                  ? `${pool} has not run a job in this window.`
                  : 'The daemon adds to this on every pass, once a job lands on a runner.'}
              </Text>
            </Stack>
          </Center>
        ) : (
          <BarChart
            h={narrow ? 200 : 220}
            data={data}
            dataKey="day"
            // Whole days, so a bar sits on a date rather than between two.
            // Nothing is interpolated: a day with no work has no bar, which is
            // the truth about that day rather than a gap in a line.
            series={[{ name: 'Runner-hours', color: palette.bar }]}
            yAxisProps={{ width: 44 }}
            xAxisProps={{
              tickFormatter: (value: string) =>
                new Date(`${value}T00:00:00Z`).toLocaleDateString([], {
                  day: 'numeric',
                  month: 'short',
                }),
              // Wide enough apart that two dates never touch. A phone fits
              // about three of them, which is enough to read the window.
              minTickGap: narrow ? 56 : 24,
            }}
            valueFormatter={(value: number) => `${value.toFixed(1)} h`}
            // The colours go in as CSS variables rather than through the
            // `gridColor` and `textColor` props: @mantine/charts 9.5.1 forwards
            // those to the root div, where React reports them as unknown
            // attributes. These are the same two variables they would have set.
            vars={() => ({
              root: { '--chart-grid-color': palette.grid, '--chart-text-color': palette.muted },
            })}
            tooltipAnimationDuration={120}
            tooltipProps={{
              labelFormatter: (value: unknown) =>
                new Date(`${String(value)}T00:00:00Z`).toLocaleDateString([], {
                  weekday: 'short',
                  day: 'numeric',
                  month: 'short',
                }),
            }}
          />
        )}
      </Card>

      {rows.length === 0 ? (
        <Card withBorder padding="md">
          <Text size="sm" c="dimmed" ta="center" py="md">
            There are no pools on this host yet.
          </Text>
        </Card>
      ) : narrow ? (
        <Stack gap="sm">
          {rows.map((row) => (
            <PoolCard key={row.pool} row={row} />
          ))}
        </Stack>
      ) : (
        <Card withBorder padding={0}>
          <Table highlightOnHover verticalSpacing="sm" horizontalSpacing="md">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Pool</Table.Th>
                <Table.Th>Jobs</Table.Th>
                <Table.Th>Time on jobs</Table.Th>
                <Table.Th>Mean job</Table.Th>
                <Table.Th>Busiest day</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((row) => (
                <PoolRow key={row.pool} row={row} />
              ))}
            </Table.Tbody>
          </Table>
        </Card>
      )}
    </Stack>
  )
}

function PoolRow({ row }: { row: Row }) {
  return (
    <Table.Tr>
      <Table.Td>
        <PoolName row={row} />
      </Table.Td>
      <Table.Td>{row.jobs}</Table.Td>
      <Table.Td>{duration(row.seconds)}</Table.Td>
      <Table.Td>{meanJob(row)}</Table.Td>
      <Table.Td>
        <Busiest row={row} />
      </Table.Td>
    </Table.Tr>
  )
}

/** One pool's window, as a card — the five columns will not fit on a phone. */
function PoolCard({ row }: { row: Row }) {
  return (
    <Card withBorder padding="md">
      <Stack gap="xs">
        <PoolName row={row} />
        <Field label="Jobs">
          <Text size="sm">{row.jobs}</Text>
        </Field>
        <Field label="Time on jobs">
          <Text size="sm">{duration(row.seconds)}</Text>
        </Field>
        <Field label="Mean job">
          <Text size="sm">{meanJob(row)}</Text>
        </Field>
        <Field label="Busiest day">
          <Busiest row={row} justify="flex-end" />
        </Field>
      </Stack>
    </Card>
  )
}

function PoolName({ row }: { row: Row }) {
  return (
    <Group gap={6} align="baseline">
      <Text size="sm" fw={500}>
        {row.pool}
      </Text>
      {row.gone && (
        // Kept in the tally on purpose: what a pool cost before somebody
        // removed it is exactly the sort of thing an audit is for.
        <Text size="xs" c="dimmed">
          deleted
        </Text>
      )}
    </Group>
  )
}

function Busiest({ row, justify = 'flex-start' }: { row: Row; justify?: string }) {
  if (!row.busiest) {
    return (
      <Text size="sm" c="dimmed">
        —
      </Text>
    )
  }
  return (
    <Group gap={6} align="baseline" justify={justify} wrap="nowrap">
      <Text size="sm">
        {new Date(`${row.busiest.day}T00:00:00Z`).toLocaleDateString([], {
          day: 'numeric',
          month: 'short',
        })}
      </Text>
      <Text size="xs" c="dimmed">
        {duration(row.busiest.seconds)}
      </Text>
    </Group>
  )
}

/**
 * Time on jobs over jobs counted.
 *
 * A pool that ran nothing has no mean rather than a zero: dividing by no jobs
 * would say every job took no time, which is the opposite of what happened.
 */
function meanJob(row: Row): string {
  return row.jobs > 0 ? duration(row.seconds / row.jobs) : '—'
}

/** One line of the table: a pool's window, and the day it worked hardest. */
interface Row extends PoolJobs {
  busiest?: JobDay
  /** The tally outlived the pool. Its work still happened. */
  gone: boolean
}

/**
 * The pools to show, which is not the same as the pools that ran something.
 *
 * A pool that ran nothing all month is listed with zeroes, because that is the
 * clearest possible case for making it smaller and it would otherwise be
 * invisible. A pool that has since been deleted is listed too: the host paid
 * for its work whether or not it still exists.
 */
function poolRows(totals: PoolJobs[], days: JobDay[], pools: Pool[], only: string | null): Row[] {
  const busiest = new Map<string, JobDay>()
  for (const day of days) {
    const best = busiest.get(day.pool)
    if (!best || day.seconds > best.seconds) busiest.set(day.pool, day)
  }

  const configured = new Set(pools.map((p) => p.name))
  const rows: Row[] = totals.map((total) => ({
    ...total,
    busiest: busiest.get(total.pool),
    gone: !configured.has(total.pool),
  }))

  const counted = new Set(totals.map((total) => total.pool))
  for (const p of pools) {
    if (!counted.has(p.name)) rows.push({ pool: p.name, jobs: 0, seconds: 0, gone: false })
  }
  return rows.filter((row) => !only || row.pool === only)
}

function Fact({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <div>
      <Text size="xs" c="dimmed">
        {label}
      </Text>
      <Group gap={6} align="baseline">
        <Text fz={24} fw={600}>
          {value}
        </Text>
        {hint && (
          <Text size="xs" c="dimmed">
            {hint}
          </Text>
        )}
      </Group>
    </div>
  )
}

/**
 * A span of time in the unit somebody would say it in.
 *
 * One unit, never two: "3.5 h" rather than "3 h 30 min". These are sampled
 * figures and writing them to the minute would claim a precision the daemon
 * cannot see, on top of being harder to compare down a column.
 */
export function duration(seconds: number): string {
  if (!seconds || seconds <= 0) return '0s'
  if (seconds < 60) return `${Math.round(seconds)}s`
  if (seconds < 3600) return `${Math.round(seconds / 60)} min`
  const hours = seconds / 3600
  return `${hours.toFixed(hours >= 100 ? 0 : 1)} h`
}
