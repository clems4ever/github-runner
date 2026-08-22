import { useState } from 'react'
import {
  ActionIcon,
  Alert,
  Badge,
  Button,
  Card,
  Center,
  Group,
  Menu,
  Modal,
  Stack,
  Switch,
  Table,
  Text,
  Title,
  Tooltip,
} from '@mantine/core'
import { notifications } from '@mantine/notifications'
import {
  IconAlertTriangle,
  IconDots,
  IconDownload,
  IconFileImport,
  IconPencil,
  IconPlus,
  IconStack2,
  IconTrash,
} from '@tabler/icons-react'
import {
  api,
  effectiveLabels,
  emptyPool,
  type Credential,
  type Pool,
  type Runner,
  type Scale,
} from '../api'
import { Field, useNarrow } from '../responsive'
import { ImportPools } from './ImportPools'
import { PoolEditor } from './PoolEditor'

export function PoolsPage({
  pools,
  credentials,
  runners,
  scaling,
  onChange,
}: {
  pools: Pool[]
  credentials: Credential[]
  runners: Runner[]
  scaling: Record<string, Scale>
  onChange: () => Promise<void>
}) {
  const narrow = useNarrow()
  const [editing, setEditing] = useState<Partial<Pool> | null>(null)
  const [deleting, setDeleting] = useState<Pool | null>(null)
  const [importing, setImporting] = useState(false)

  const runnersOf = (pool: Pool) => runners.filter((r) => r.pool === pool.name)

  // Saved as a file rather than shown: this is something to keep next to a
  // repository, so the next host can be set up by importing it.
  const exportPools = async () => {
    try {
      const document = await api.exportPools()
      const url = URL.createObjectURL(new Blob([document], { type: 'application/json' }))
      const link = window.document.createElement('a')
      link.href = url
      link.download = 'runner-fleet-pools.json'
      link.click()
      URL.revokeObjectURL(url)
    } catch (error) {
      notifications.show({
        color: 'red',
        title: 'Could not export the pools',
        message: error instanceof Error ? error.message : String(error),
      })
    }
  }

  return (
    <Stack gap="lg">
      {/* On a phone the three actions take the whole row under the heading
          rather than being squeezed alongside it. */}
      <Group justify="space-between" gap="sm" wrap="wrap">
        <Title order={3}>Pools</Title>
        <Group gap="xs" wrap="wrap" style={narrow ? { flex: '1 1 100%' } : undefined}>
          <Button
            variant="default"
            leftSection={<IconDownload size={16} />}
            disabled={pools.length === 0}
            onClick={exportPools}
          >
            Export
          </Button>
          <Button
            variant="default"
            leftSection={<IconFileImport size={16} />}
            disabled={credentials.length === 0}
            onClick={() => setImporting(true)}
          >
            Import
          </Button>
          <Button
            leftSection={<IconPlus size={16} />}
            disabled={credentials.length === 0}
            onClick={() => setEditing(emptyPool(credentials[0]?.id ?? 0))}
          >
            New pool
          </Button>
        </Group>
      </Group>

      {credentials.length === 0 && (
        <Alert color="yellow" variant="light" icon={<IconAlertTriangle size={18} />}>
          Add a credential first. A runner registers afresh every time it starts, so a pool needs a
          token that can mint registration tokens.
        </Alert>
      )}

      {pools.length === 0 ? (
        <Card withBorder padding="xl">
          <Center>
            <Stack align="center" gap="xs">
              <IconStack2 size={32} stroke={1.2} opacity={0.5} />
              <Text fw={500}>No pools yet</Text>
              <Text size="sm" c="dimmed" ta="center" maw={420}>
                A pool is a set of identical runners on one repository or organisation. Scaling it
                is changing one number.
              </Text>
            </Stack>
          </Center>
        </Card>
      ) : narrow ? (
        <Stack gap="sm">
          {pools.map((pool) => (
            <PoolCard
              key={pool.id}
              pool={pool}
              live={runnersOf(pool).length}
              decision={scaling[pool.name]}
              onChange={onChange}
              onEdit={() => setEditing(pool)}
              onDelete={() => setDeleting(pool)}
            />
          ))}
        </Stack>
      ) : (
        <Card withBorder padding={0}>
          <Table highlightOnHover verticalSpacing="sm" horizontalSpacing="md">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Pool</Table.Th>
                <Table.Th>Scope</Table.Th>
                <Table.Th>Runtime</Table.Th>
                <Table.Th>Runners</Table.Th>
                <Table.Th>Labels</Table.Th>
                <Table.Th>Size</Table.Th>
                <Table.Th w={100}>Enabled</Table.Th>
                <Table.Th w={48} />
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {pools.map((pool) => {
                const live = runnersOf(pool)
                return (
                  <Table.Tr key={pool.id}>
                    <Table.Td>
                      <Text fw={500}>{pool.name}</Text>
                    </Table.Td>
                    <Table.Td>
                      <ScopeCell pool={pool} />
                    </Table.Td>
                    <Table.Td>
                      <RuntimeBadges pool={pool} />
                    </Table.Td>
                    <Table.Td>
                      <RunnersCell pool={pool} live={live.length} decision={scaling[pool.name]} />
                    </Table.Td>
                    <Table.Td>
                      <LabelBadges pool={pool} />
                    </Table.Td>
                    <Table.Td>
                      <SizeText pool={pool} />
                    </Table.Td>
                    <Table.Td>
                      <EnabledSwitch pool={pool} onChange={onChange} />
                    </Table.Td>
                    <Table.Td>
                      <RowMenu
                        pool={pool}
                        onEdit={() => setEditing(pool)}
                        onDelete={() => setDeleting(pool)}
                      />
                    </Table.Td>
                  </Table.Tr>
                )
              })}
            </Table.Tbody>
          </Table>
        </Card>
      )}

      <Modal
        opened={editing !== null}
        onClose={() => setEditing(null)}
        title={editing?.id ? `Edit ${editing.name}` : 'New pool'}
        size="lg"
        // A long form in a floating panel on a phone is a small window inside a
        // small window; it gets the whole screen instead.
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

      <Modal
        opened={importing}
        onClose={() => setImporting(false)}
        title="Import pools from a template"
        size="xl"
        fullScreen={narrow}
      >
        <ImportPools
          credentials={credentials}
          onCancel={() => setImporting(false)}
          onImported={async () => {
            setImporting(false)
            await onChange()
          }}
        />
      </Modal>

      <Modal opened={deleting !== null} onClose={() => setDeleting(null)} title="Delete pool">
        <Stack>
          <Text size="sm">
            Delete <b>{deleting?.name}</b>? Its runners are drained rather than killed: each one
            finishes the job it is on before it goes away.
          </Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={() => setDeleting(null)}>
              Cancel
            </Button>
            <Button
              color="red"
              onClick={async () => {
                if (!deleting) return
                try {
                  await api.deletePool(deleting.id)
                  setDeleting(null)
                  await onChange()
                } catch (error) {
                  notifications.show({
                    color: 'red',
                    title: 'Could not delete the pool',
                    message: error instanceof Error ? error.message : String(error),
                  })
                }
              }}
            >
              Delete
            </Button>
          </Group>
        </Stack>
      </Modal>
    </Stack>
  )
}

/**
 * One pool, as a card.
 *
 * The row menu is the thing that most needed rescuing: in a table this narrow
 * it sits in the eighth column, which is off the edge of a phone, so editing or
 * deleting a pool was simply not reachable. Here it is beside the name.
 */
function PoolCard({
  pool,
  live,
  decision,
  onChange,
  onEdit,
  onDelete,
}: {
  pool: Pool
  live: number
  decision?: Scale
  onChange: () => Promise<void>
  onEdit: () => void
  onDelete: () => void
}) {
  return (
    <Card withBorder padding="md">
      <Stack gap="xs">
        <Group justify="space-between" gap="xs" wrap="nowrap" align="flex-start">
          <Text fw={500} style={{ wordBreak: 'break-word' }}>
            {pool.name}
          </Text>
          <Group gap="xs" wrap="nowrap" style={{ flexShrink: 0 }}>
            <EnabledSwitch pool={pool} onChange={onChange} />
            <RowMenu pool={pool} onEdit={onEdit} onDelete={onDelete} />
          </Group>
        </Group>
        <Field label="Scope">
          <ScopeCell pool={pool} />
        </Field>
        <Field label="Runtime">
          <RuntimeBadges pool={pool} />
        </Field>
        <Field label="Runners">
          <RunnersCell pool={pool} live={live} decision={decision} />
        </Field>
        <Field label="Labels">
          <LabelBadges pool={pool} />
        </Field>
        <Field label="Size">
          <SizeText pool={pool} />
        </Field>
      </Stack>
    </Card>
  )
}

function ScopeCell({ pool }: { pool: Pool }) {
  return (
    <Group gap={6} justify="flex-end">
      <Text size="sm" style={{ wordBreak: 'break-word' }}>
        {pool.scope}
      </Text>
      {pool.scopeKind === 'organization' && (
        <Badge size="xs" variant="default">
          org
        </Badge>
      )}
    </Group>
  )
}

function RuntimeBadges({ pool }: { pool: Pool }) {
  return (
    <Group gap={4} justify="flex-end">
      <Badge variant="default" size="sm">
        {pool.runtime}
      </Badge>
      {pool.nested && (
        <Tooltip label="Jobs can boot machines of their own">
          <Badge size="sm" color="grape" variant="light">
            nestedvirt
          </Badge>
        </Tooltip>
      )}
    </Group>
  )
}

function RunnersCell({ pool, live, decision }: { pool: Pool; live: number; decision?: Scale }) {
  return (
    <Tooltip label={decision?.reason ?? 'no decision recorded yet'} disabled={!decision}>
      <Group gap={6} wrap="nowrap" justify="flex-end">
        <Text size="sm" fw={500}>
          {live}
        </Text>
        <Text size="sm" c="dimmed">
          {pool.minReplicas === pool.maxReplicas
            ? `/ ${pool.maxReplicas}`
            : `of ${pool.minReplicas}–${pool.maxReplicas}`}
        </Text>
        {decision?.scaledUp && (
          <Badge size="xs" color="blue" variant="light">
            scaling up
          </Badge>
        )}
      </Group>
    </Tooltip>
  )
}

function LabelBadges({ pool }: { pool: Pool }) {
  return (
    <Group gap={4} justify="flex-end">
      {effectiveLabels(pool).map((label) => (
        <Badge key={label} size="xs" variant="dot">
          {label}
        </Badge>
      ))}
    </Group>
  )
}

function SizeText({ pool }: { pool: Pool }) {
  return (
    <Text size="sm" c="dimmed">
      {pool.cpus} vCPU · {Math.round(pool.memoryMb / 1024)} GiB
      {pool.runtime === 'vm' ? ` · ${pool.diskGb} GiB` : ''}
    </Text>
  )
}

function EnabledSwitch({ pool, onChange }: { pool: Pool; onChange: () => Promise<void> }) {
  return (
    <Switch
      checked={pool.enabled}
      aria-label={`Enable ${pool.name}`}
      onChange={async (event) => {
        try {
          await api.updatePool(pool.id, { ...pool, enabled: event.currentTarget.checked })
          await onChange()
        } catch (error) {
          notifications.show({
            color: 'red',
            title: 'Could not change the pool',
            message: error instanceof Error ? error.message : String(error),
          })
        }
      }}
    />
  )
}

function RowMenu({
  pool,
  onEdit,
  onDelete,
}: {
  pool: Pool
  onEdit: () => void
  onDelete: () => void
}) {
  return (
    <Menu position="bottom-end">
      <Menu.Target>
        <ActionIcon variant="subtle" aria-label={`Actions for ${pool.name}`}>
          <IconDots size={16} />
        </ActionIcon>
      </Menu.Target>
      <Menu.Dropdown>
        <Menu.Item leftSection={<IconPencil size={14} />} onClick={onEdit}>
          Edit
        </Menu.Item>
        <Menu.Item color="red" leftSection={<IconTrash size={14} />} onClick={onDelete}>
          Delete
        </Menu.Item>
      </Menu.Dropdown>
    </Menu>
  )
}
