import { useState } from 'react'
import {
  ActionIcon, Alert, Badge, Button, Card, Center, Group, Menu, Modal,
  Stack, Table, Text, Title, Tooltip,
} from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { IconDots, IconKey, IconPlus, IconRefresh, IconTrash } from '@tabler/icons-react'
import { api, type Credential, type Pool } from '../api'
import { Field, useNarrow } from '../responsive'
import { CredentialForm } from './CredentialForm'

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
  const narrow = useNarrow()
  const [adding, setAdding] = useState(false)
  const [rotating, setRotating] = useState<Credential | null>(null)

  const usedBy = (credential: Credential) => pools.filter((p) => p.credentialId === credential.id)

  const close = () => {
    setAdding(false)
    setRotating(null)
  }

  return (
    <Stack gap="lg">
      <Group justify="space-between" gap="sm" wrap="wrap">
        <Title order={3}>Credentials</Title>
        <Button
          leftSection={<IconPlus size={16} />}
          onClick={() => { setAdding(true); setRotating(null) }}
        >
          Add credential
        </Button>
      </Group>

      <Alert variant="light" color="blue">
        A <b>GitHub App</b> is the better credential: nothing expires on a calendar, the
        repositories it can reach are a list you edit, and uninstalling it revokes everything at
        once. A <b>personal access token</b> works too. Either needs{' '}
        <b>Administration: Read and write</b> on the repositories it covers, or{' '}
        <b>Self-hosted runners: Read and write</b> on an organisation — and either is encrypted at
        rest and never shown again.
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
      ) : narrow ? (
        <Stack gap="sm">
          {credentials.map((credential) => (
            <CredentialCard
              key={credential.id}
              credential={credential}
              usedBy={usedBy(credential)}
              onRotate={() => { setRotating(credential); setAdding(false) }}
              onChange={onChange}
            />
          ))}
        </Stack>
      ) : (
        <Card withBorder padding={0}>
          <Table highlightOnHover verticalSpacing="sm" horizontalSpacing="md">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Name</Table.Th>
                <Table.Th>Kind</Table.Th>
                <Table.Th>Identifier</Table.Th>
                <Table.Th>Used by</Table.Th>
                <Table.Th w={48} />
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {credentials.map((credential) => (
                <Table.Tr key={credential.id}>
                  <Table.Td><Text fw={500}>{credential.name}</Text></Table.Td>
                  <Table.Td><KindBadge kind={credential.kind} /></Table.Td>
                  <Table.Td><Text ff="monospace" size="sm" c="dimmed">{credential.hint}</Text></Table.Td>
                  <Table.Td>
                    <Text size="sm" c="dimmed">
                      {usedBy(credential).map((p) => p.name).join(', ') || '—'}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <CredentialMenu
                      credential={credential}
                      usedBy={usedBy(credential)}
                      onRotate={() => { setRotating(credential); setAdding(false) }}
                      onChange={onChange}
                    />
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Card>
      )}

      <Modal
        opened={adding || rotating !== null}
        onClose={close}
        title={
          rotating
            ? `Replace the ${rotating.kind === 'app' ? 'private key' : 'token'} for ${rotating.name}`
            : 'Add credential'
        }
        size="lg"
        fullScreen={narrow}
      >
        <CredentialForm
          rotating={rotating}
          onCancel={close}
          onSaved={async () => {
            close()
            await onChange()
          }}
        />
      </Modal>
    </Stack>
  )
}

/**
 * One credential, as a card.
 *
 * As on the pools page, the menu that rotates and deletes is what a narrow
 * table loses first, so here it sits beside the name rather than in a column
 * past the edge of the screen.
 */
function CredentialCard({
  credential,
  usedBy,
  onRotate,
  onChange,
}: {
  credential: Credential
  usedBy: Pool[]
  onRotate: () => void
  onChange: () => Promise<void>
}) {
  return (
    <Card withBorder padding="md">
      <Stack gap="xs">
        <Group justify="space-between" gap="xs" wrap="nowrap" align="flex-start">
          <Text fw={500} style={{ wordBreak: 'break-word' }}>{credential.name}</Text>
          <CredentialMenu
            credential={credential}
            usedBy={usedBy}
            onRotate={onRotate}
            onChange={onChange}
          />
        </Group>
        <Field label="Kind"><KindBadge kind={credential.kind} /></Field>
        <Field label="Identifier">
          <Text ff="monospace" size="sm" c="dimmed">{credential.hint}</Text>
        </Field>
        <Field label="Used by">
          <Text size="sm" c="dimmed">{usedBy.map((p) => p.name).join(', ') || '—'}</Text>
        </Field>
      </Stack>
    </Card>
  )
}

function KindBadge({ kind }: { kind: Credential['kind'] }) {
  if (kind === 'app') {
    return (
      <Tooltip label="Signs its own short-lived tokens; nothing expires on a calendar">
        <Badge variant="light" size="sm">app</Badge>
      </Tooltip>
    )
  }
  return <Badge variant="default" size="sm">token</Badge>
}

function CredentialMenu({
  credential,
  usedBy,
  onRotate,
  onChange,
}: {
  credential: Credential
  usedBy: Pool[]
  onRotate: () => void
  onChange: () => Promise<void>
}) {
  return (
    <Menu position="bottom-end">
      <Menu.Target>
        <ActionIcon variant="subtle" aria-label={`Actions for ${credential.name}`}>
          <IconDots size={16} />
        </ActionIcon>
      </Menu.Target>
      <Menu.Dropdown>
        <Menu.Item leftSection={<IconRefresh size={14} />} onClick={onRotate}>
          {credential.kind === 'app' ? 'Replace private key' : 'Replace token'}
        </Menu.Item>
        <Menu.Item
          color="red"
          leftSection={<IconTrash size={14} />}
          disabled={usedBy.length > 0}
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
  )
}
