import { useState } from 'react'
import {
  ActionIcon, Alert, Button, Card, Center, Group, Menu, Modal, PasswordInput,
  Stack, Table, Text, TextInput, Title,
} from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { IconDots, IconKey, IconPlus, IconRefresh, IconTrash } from '@tabler/icons-react'
import { api, type Credential, type Pool } from '../api'

/**
 * Credentials.
 *
 * A token goes in once and never comes back out: the daemon seals it, and the
 * list only ever shows the last four characters, which is enough to tell two
 * apart and useless to anyone reading over a shoulder.
 */
export function CredentialsPage({
  credentials, pools, onChange,
}: { credentials: Credential[]; pools: Pool[]; onChange: () => Promise<void> }) {
  const [adding, setAdding] = useState(false)
  const [rotating, setRotating] = useState<Credential | null>(null)
  const [name, setName] = useState('')
  const [token, setToken] = useState('')
  const [saving, setSaving] = useState(false)

  const usedBy = (credential: Credential) => pools.filter((p) => p.credentialId === credential.id)

  const submit = async () => {
    setSaving(true)
    try {
      if (rotating) {
        await api.rotateCredential(rotating.id, token)
        notifications.show({
          color: 'green',
          title: 'Token replaced',
          message: 'Runners using it are replaced gracefully, as they finish their jobs.',
        })
      } else {
        await api.createCredential(name, token)
      }
      setAdding(false); setRotating(null); setName(''); setToken('')
      await onChange()
    } catch (error) {
      notifications.show({
        color: 'red',
        title: 'Could not save the credential',
        message: error instanceof Error ? error.message : String(error),
      })
    } finally {
      setSaving(false)
    }
  }

  return (
    <Stack gap="lg">
      <Group justify="space-between">
        <Title order={3}>Credentials</Title>
        <Button leftSection={<IconPlus size={16} />} onClick={() => { setAdding(true); setRotating(null) }}>
          Add credential
        </Button>
      </Group>

      <Alert variant="light" color="blue">
        A fine-grained token needs <b>Administration: Read and write</b> on a repository, or{' '}
        <b>Self-hosted runners: Read and write</b> on an organisation. It is encrypted at rest and
        never shown again.
      </Alert>

      {credentials.length === 0 ? (
        <Card withBorder padding="xl">
          <Center>
            <Stack align="center" gap="xs">
              <IconKey size={32} stroke={1.2} opacity={0.5} />
              <Text fw={500}>No credentials yet</Text>
              <Text size="sm" c="dimmed">Pools cannot register runners without one.</Text>
            </Stack>
          </Center>
        </Card>
      ) : (
        <Card withBorder padding={0}>
          <Table highlightOnHover verticalSpacing="sm" horizontalSpacing="md">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Name</Table.Th>
                <Table.Th>Token</Table.Th>
                <Table.Th>Used by</Table.Th>
                <Table.Th w={48} />
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {credentials.map((credential) => (
                <Table.Tr key={credential.id}>
                  <Table.Td><Text fw={500}>{credential.name}</Text></Table.Td>
                  <Table.Td><Text ff="monospace" size="sm" c="dimmed">{credential.hint}</Text></Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed">
                      {usedBy(credential).map((p) => p.name).join(', ') || '—'}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Menu position="bottom-end">
                      <Menu.Target>
                        <ActionIcon variant="subtle" aria-label={`Actions for ${credential.name}`}>
                          <IconDots size={16} />
                        </ActionIcon>
                      </Menu.Target>
                      <Menu.Dropdown>
                        <Menu.Item
                          leftSection={<IconRefresh size={14} />}
                          onClick={() => { setRotating(credential); setToken(''); setAdding(false) }}
                        >
                          Replace token
                        </Menu.Item>
                        <Menu.Item
                          color="red"
                          leftSection={<IconTrash size={14} />}
                          disabled={usedBy(credential).length > 0}
                          onClick={async () => {
                            try {
                              await api.deleteCredential(credential.id)
                              await onChange()
                            } catch (error) {
                              notifications.show({
                                color: 'red',
                                title: 'Could not delete the credential',
                                message: error instanceof Error ? error.message : String(error),
                              })
                            }
                          }}
                        >
                          Delete
                        </Menu.Item>
                      </Menu.Dropdown>
                    </Menu>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Card>
      )}

      <Modal
        opened={adding || rotating !== null}
        onClose={() => { setAdding(false); setRotating(null) }}
        title={rotating ? `Replace the token for ${rotating.name}` : 'Add credential'}
      >
        <Stack>
          {!rotating && (
            <TextInput
              label="Name" placeholder="personal access token" withAsterisk
              value={name} onChange={(event) => setName(event.currentTarget.value)}
            />
          )}
          <PasswordInput
            label="Token" placeholder="github_pat_..." withAsterisk
            value={token} onChange={(event) => setToken(event.currentTarget.value)}
          />
          {rotating && (
            <Text size="xs" c="dimmed">
              Pools using this credential will replace their runners gracefully, as each finishes
              the job it is on.
            </Text>
          )}
          <Group justify="flex-end">
            <Button variant="default" onClick={() => { setAdding(false); setRotating(null) }}>
              Cancel
            </Button>
            <Button onClick={submit} loading={saving} disabled={!token || (!rotating && !name)}>
              Save
            </Button>
          </Group>
        </Stack>
      </Modal>
    </Stack>
  )
}
