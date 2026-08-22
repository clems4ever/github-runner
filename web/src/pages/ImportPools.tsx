import { useState } from 'react'
import {
  Alert,
  Anchor,
  Badge,
  Button,
  Card,
  Checkbox,
  FileButton,
  Group,
  SegmentedControl,
  Select,
  SimpleGrid,
  Stack,
  Table,
  Text,
  Textarea,
  TextInput,
} from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { IconAlertTriangle, IconExternalLink, IconFileImport } from '@tabler/icons-react'
import {
  api,
  ApiError,
  type Credential,
  type ImportOutcome,
  type ImportReport,
  type ScopeKind,
} from '../api'
import { Field, useNarrow } from '../responsive'

/**
 * Importing a template.
 *
 * The preview is not a guess. It is the import itself, run against the database
 * and rolled back, so a line saying "create" is a pool that was created a
 * moment ago and then undone. That is why it is worth reading before pressing
 * the button underneath it.
 */
export function ImportPools({
  credentials,
  onImported,
  onCancel,
}: {
  credentials: Credential[]
  onImported: () => Promise<void>
  onCancel: () => void
}) {
  const narrow = useNarrow()
  const [text, setText] = useState('')
  const [credentialId, setCredentialId] = useState<number>(credentials[0]?.id ?? 0)
  const [scope, setScope] = useState('')
  const [scopeKind, setScopeKind] = useState<ScopeKind>('repository')
  const [replaceExisting, setReplaceExisting] = useState(false)
  const [report, setReport] = useState<ImportReport | null>(null)
  const [problem, setProblem] = useState<{ message: string; grantUrl?: string } | null>(null)
  const [busy, setBusy] = useState(false)

  // Anything typed invalidates a preview taken before it, so the button under
  // the table always belongs to the table above it.
  const change = (value: string) => {
    setText(value)
    setReport(null)
    setProblem(null)
  }

  const send = async (dryRun: boolean) => {
    let parsed: unknown
    try {
      parsed = JSON.parse(text)
    } catch (error) {
      // The only check made here. Everything about what the document *means*
      // is the daemon's answer, so there is one set of rules rather than two.
      setProblem({ message: `This is not valid JSON: ${(error as Error).message}` })
      return
    }

    setBusy(true)
    setProblem(null)
    try {
      const result = await api.importPools({
        document: parsed,
        credentialId,
        scope: scope.trim() || undefined,
        scopeKind: scope.trim() ? scopeKind : undefined,
        replaceExisting,
        dryRun,
      })
      if (dryRun) {
        setReport(result)
        return
      }
      notifications.show({
        color: 'green',
        title: `Imported ${result.pools.length} ${result.pools.length === 1 ? 'pool' : 'pools'}`,
        message: 'Their runners are started on the next pass, which is now.',
      })
      await onImported()
    } catch (error) {
      if (error instanceof ApiError) {
        setProblem({ message: error.message, grantUrl: error.grantUrl })
      } else {
        setProblem({ message: error instanceof Error ? error.message : String(error) })
      }
      setReport(null)
    } finally {
      setBusy(false)
    }
  }

  const ready = text.trim() !== '' && credentialId > 0

  return (
    <Stack>
      <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="md" verticalSpacing="md">
        <Select
          label="Credential"
          description="Imported pools register with it"
          withAsterisk
          data={credentials.map((c) => ({ value: String(c.id), label: `${c.name} (${c.hint})` }))}
          value={credentialId ? String(credentialId) : null}
          onChange={(value) => {
            setCredentialId(Number(value))
            setReport(null)
          }}
        />
        <TextInput
          label="Scope"
          description="Optional — replaces the scope of every pool in the template"
          placeholder="owner/repository"
          value={scope}
          onChange={(event) => {
            setScope(event.currentTarget.value)
            setReport(null)
          }}
        />
      </SimpleGrid>

      {scope.trim() !== '' && (
        <SegmentedControl
          size="xs"
          fullWidth={narrow}
          value={scopeKind}
          onChange={(value) => setScopeKind(value as ScopeKind)}
          data={[
            { value: 'repository', label: 'Repository' },
            { value: 'organization', label: 'Organisation' },
          ]}
        />
      )}

      <Textarea
        label="Template"
        description="Paste a template, or choose the file"
        withAsterisk
        // Ten rows is most of a phone screen before the buttons under it come
        // into view.
        rows={narrow ? 6 : 10}
        autosize={false}
        placeholder={'{\n  "version": 1,\n  "pools": [ … ]\n}'}
        styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)', fontSize: 12 } }}
        value={text}
        onChange={(event) => change(event.currentTarget.value)}
      />

      <Group justify="space-between" gap="sm" wrap="wrap">
        <FileButton
          accept="application/json"
          onChange={async (file) => {
            if (file) change(await file.text())
          }}
        >
          {(props) => (
            <Button {...props} variant="default" size="xs" leftSection={<IconFileImport size={14} />}>
              Choose a file
            </Button>
          )}
        </FileButton>
        <Checkbox
          label="Import over pools that already exist"
          checked={replaceExisting}
          onChange={(event) => {
            setReplaceExisting(event.currentTarget.checked)
            setReport(null)
          }}
        />
      </Group>

      {problem && (
        <Alert color="red" variant="light" icon={<IconAlertTriangle size={18} />}>
          <Text size="sm">{problem.message}</Text>
          {problem.grantUrl && (
            <Anchor size="sm" href={problem.grantUrl} target="_blank" rel="noreferrer" mt={6} display="block">
              Grant access on GitHub <IconExternalLink size={12} />
            </Anchor>
          )}
        </Alert>
      )}

      {report && (
        <Stack gap="xs">
          {report.name && (
            <Text size="sm" fw={500}>
              {report.name}
            </Text>
          )}
          {report.description && (
            <Text size="xs" c="dimmed">
              {report.description}
            </Text>
          )}
          {/* This preview is the point of the screen — what would happen, read
              before pressing the button under it — so on a phone it becomes a
              card per pool rather than a table with four columns off the edge. */}
          {narrow ? (
            <Stack gap="xs">
              {report.pools.map(({ name, action, pool }) => (
                <Card key={name} withBorder padding="sm">
                  <Stack gap={6}>
                    <Group justify="space-between" gap="xs" wrap="nowrap" align="flex-start">
                      <Text size="sm" fw={500} style={{ wordBreak: 'break-word' }}>
                        {name}
                      </Text>
                      <ActionBadge action={action} />
                    </Group>
                    <Field label="Scope">
                      <Text size="sm" style={{ wordBreak: 'break-word' }}>
                        {pool.scope}
                      </Text>
                    </Field>
                    <Field label="Runtime">
                      <Badge size="sm" variant="default">
                        {pool.runtime}
                      </Badge>
                    </Field>
                    <Field label="Size">
                      <SizeText pool={pool} />
                    </Field>
                    <Field label="Runners">
                      <RunnersText pool={pool} />
                    </Field>
                  </Stack>
                </Card>
              ))}
            </Stack>
          ) : (
            <Table withTableBorder verticalSpacing="xs" horizontalSpacing="sm">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Pool</Table.Th>
                  <Table.Th>Scope</Table.Th>
                  <Table.Th>Runtime</Table.Th>
                  <Table.Th>Size</Table.Th>
                  <Table.Th>Runners</Table.Th>
                  <Table.Th />
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {report.pools.map(({ name, action, pool }) => (
                  <Table.Tr key={name}>
                    <Table.Td>
                      <Text size="sm" fw={500}>
                        {name}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Text size="sm">{pool.scope}</Text>
                    </Table.Td>
                    <Table.Td>
                      <Badge size="sm" variant="default">
                        {pool.runtime}
                      </Badge>
                    </Table.Td>
                    <Table.Td>
                      <SizeText pool={pool} />
                    </Table.Td>
                    <Table.Td>
                      <RunnersText pool={pool} />
                    </Table.Td>
                    <Table.Td>
                      <ActionBadge action={action} />
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          )}
          {report.pools.some((outcome) => outcome.action === 'update') && (
            <Text size="xs" c="dimmed">
              Pools written over keep their identity and their runners: each runner is replaced
              gracefully, as it finishes the job it is on.
            </Text>
          )}
        </Stack>
      )}

      <Group justify="flex-end" gap="sm" wrap="wrap">
        <Button variant="default" onClick={onCancel}>
          Cancel
        </Button>
        {report ? (
          <Button loading={busy} onClick={() => send(false)}>
            Import {report.pools.length} {report.pools.length === 1 ? 'pool' : 'pools'}
          </Button>
        ) : (
          <Button loading={busy} disabled={!ready} onClick={() => send(true)}>
            Preview
          </Button>
        )}
      </Group>
    </Stack>
  )
}

function SizeText({ pool }: { pool: ImportOutcome['pool'] }) {
  return (
    <Text size="sm" c="dimmed">
      {pool.cpus} vCPU · {Math.round(pool.memoryMb / 1024)} GiB
      {pool.runtime === 'vm' ? ` · ${pool.diskGb} GiB` : ''}
    </Text>
  )
}

function RunnersText({ pool }: { pool: ImportOutcome['pool'] }) {
  return (
    <Text size="sm" c="dimmed">
      {pool.minReplicas === pool.maxReplicas
        ? pool.maxReplicas
        : `${pool.minReplicas}–${pool.maxReplicas}`}
    </Text>
  )
}

function ActionBadge({ action }: { action: ImportOutcome['action'] }) {
  return (
    <Badge size="sm" color={action === 'update' ? 'yellow' : 'blue'} variant="light">
      {action === 'update' ? 'replaces the pool of this name' : 'new'}
    </Badge>
  )
}
