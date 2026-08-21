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
  IconPencil,
  IconPlus,
  IconStack2,
  IconTrash,
} from '@tabler/icons-react'
import { api, effectiveLabels, emptyPool, type Credential, type Pool, type Runner } from '../api'
import { PoolEditor } from './PoolEditor'

export function PoolsPage({
  pools,
  credentials,
  runners,
  onChange,
}: {
  pools: Pool[]
  credentials: Credential[]
  runners: Runner[]
  onChange: () => Promise<void>
}) {
  const [editing, setEditing] = useState<Partial<Pool> | null>(null)
  const [deleting, setDeleting] = useState<Pool | null>(null)

  const runnersOf = (pool: Pool) => runners.filter((r) => r.pool === pool.name)

  return (
    <Stack gap="lg">
      <Group justify="space-between">
        <Title order={3}>Pools</Title>
        <Button
          leftSection={<IconPlus size={16} />}
          disabled={credentials.length === 0}
          onClick={() => setEditing(emptyPool(credentials[0]?.id ?? 0))}
        >
          New pool
        </Button>
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
                      <Group gap={6}>
                        <Text size="sm">{pool.scope}</Text>
                        {pool.scopeKind === 'organization' && (
                          <Badge size="xs" variant="default">
                            org
                          </Badge>
                        )}
                      </Group>
                    </Table.Td>
                    <Table.Td>
                      <Group gap={4}>
                        <Badge variant="default" size="sm">
                          {pool.runtime}
                        </Badge>
                        {pool.nested && (
                          <Tooltip label="Jobs can boot machines of their own">
                            <Badge size="sm" color="grape" variant="light">
                              nested
                            </Badge>
                          </Tooltip>
                        )}
                      </Group>
                    </Table.Td>
                    <Table.Td>
                      <Text size="sm">
                        {live.length} / {pool.replicas}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Group gap={4}>
                        {effectiveLabels(pool).map((label) => (
                          <Badge key={label} size="xs" variant="dot">
                            {label}
                          </Badge>
                        ))}
                      </Group>
                    </Table.Td>
                    <Table.Td>
                      <Text size="sm" c="dimmed">
                        {pool.cpus} vCPU · {Math.round(pool.memoryMb / 1024)} GiB
                        {pool.runtime === 'vm' ? ` · ${pool.diskGb} GiB` : ''}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Switch
                        checked={pool.enabled}
                        aria-label={`Enable ${pool.name}`}
                        onChange={async (event) => {
                          try {
                            await api.updatePool(pool.id, {
                              ...pool,
                              enabled: event.currentTarget.checked,
                            })
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
                    </Table.Td>
                    <Table.Td>
                      <Menu position="bottom-end">
                        <Menu.Target>
                          <ActionIcon variant="subtle" aria-label={`Actions for ${pool.name}`}>
                            <IconDots size={16} />
                          </ActionIcon>
                        </Menu.Target>
                        <Menu.Dropdown>
                          <Menu.Item
                            leftSection={<IconPencil size={14} />}
                            onClick={() => setEditing(pool)}
                          >
                            Edit
                          </Menu.Item>
                          <Menu.Item
                            color="red"
                            leftSection={<IconTrash size={14} />}
                            onClick={() => setDeleting(pool)}
                          >
                            Delete
                          </Menu.Item>
                        </Menu.Dropdown>
                      </Menu>
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
