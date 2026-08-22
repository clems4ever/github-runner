import { useState } from 'react'
import {
  Alert,
  Badge,
  Button,
  Divider,
  Group,
  NumberInput,
  Select,
  SegmentedControl,
  Stack,
  Switch,
  TagsInput,
  Text,
  TextInput,
} from '@mantine/core'
import { useForm } from '@mantine/form'
import { notifications } from '@mantine/notifications'
import { IconAlertTriangle } from '@tabler/icons-react'
import { api, effectiveLabels, type Credential, type Pool } from '../api'

/**
 * The pool editor.
 *
 * Everything that decides what a runner is lives on one screen, because the
 * combinations matter to each other: a container with nested virtualisation is
 * a different security proposition from a machine with it, and the labels a
 * workflow will target follow from both.
 */
export function PoolEditor({
  pool,
  credentials,
  onSaved,
  onCancel,
}: {
  pool: Partial<Pool>
  credentials: Credential[]
  onSaved: () => Promise<void>
  onCancel: () => void
}) {
  const [saving, setSaving] = useState(false)

  const form = useForm<Partial<Pool>>({
    initialValues: { ...pool },
    validate: {
      name: (value) =>
        /^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$/.test(value ?? '')
          ? null
          : 'Lower-case letters, digits and dashes, not starting or ending with a dash',
      scope: (value, values) => {
        if (!value) return 'Required'
        if (values.scopeKind === 'repository' && !/^[^/]+\/[^/]+$/.test(value)) {
          return 'A repository is owner/name'
        }
        if (values.scopeKind === 'organization' && value.includes('/')) {
          return 'An organisation is a single name'
        }
        return null
      },
      // One, not zero. A pool with no runners has nothing able to accept a
      // job, so it could never discover that it needs to grow.
      minReplicas: (value) =>
        (value ?? 0) >= 1 && (value ?? 0) <= 64
          ? null
          : 'At least 1 — use the enabled switch to stop a pool entirely',
      maxReplicas: (value, values) =>
        (value ?? 0) >= (values.minReplicas ?? 1) && (value ?? 0) <= 64
          ? null
          : 'At least the minimum, and no more than 64',
    },
  })

  const values = form.values
  const isVM = values.runtime === 'vm'

  return (
    <form
      onSubmit={form.onSubmit(async (submitted) => {
        setSaving(true)
        try {
          if (pool.id) {
            await api.updatePool(pool.id, submitted)
          } else {
            await api.createPool(submitted)
          }
          await onSaved()
        } catch (error) {
          notifications.show({
            color: 'red',
            title: 'Could not save the pool',
            message: error instanceof Error ? error.message : String(error),
          })
        } finally {
          setSaving(false)
        }
      })}
    >
      <Stack gap="md">
        <Group grow align="flex-start">
          <TextInput
            label="Name"
            description="Runners are named after it: web-1, web-2"
            placeholder="web"
            withAsterisk
            {...form.getInputProps('name')}
          />
          <Select
            label="Credential"
            description="Mints a registration token per start"
            withAsterisk
            data={credentials.map((c) => ({ value: String(c.id), label: `${c.name} (${c.hint})` }))}
            value={values.credentialId ? String(values.credentialId) : null}
            onChange={(value) => form.setFieldValue('credentialId', Number(value))}
          />
        </Group>

        <div>
          <Text size="sm" fw={500} mb={4}>
            Scope
          </Text>
          <Group align="flex-start" grow>
            <SegmentedControl
              data={[
                { value: 'repository', label: 'Repository' },
                { value: 'organization', label: 'Organisation' },
              ]}
              value={values.scopeKind}
              onChange={(value) => form.setFieldValue('scopeKind', value as Pool['scopeKind'])}
            />
            <TextInput
              placeholder={values.scopeKind === 'organization' ? 'my-org' : 'owner/repository'}
              {...form.getInputProps('scope')}
            />
          </Group>
          {values.scopeKind === 'organization' && (
            <Text size="xs" c="dimmed" mt={4}>
              Every repository in the organisation can use these runners. GitHub has no equivalent
              for a personal account.
            </Text>
          )}
        </div>

        <Divider label="Runtime" labelPosition="left" />

        <SegmentedControl
          fullWidth
          data={[
            { value: 'vm', label: 'Virtual machine' },
            { value: 'container', label: 'Container' },
          ]}
          value={values.runtime}
          onChange={(value) => form.setFieldValue('runtime', value as Pool['runtime'])}
        />
        <Text size="xs" c="dimmed" mt={-8}>
          {isVM
            ? 'A machine per job: its own kernel, its own Docker daemon, nothing shared with the host.'
            : 'A container per runner: faster to start and cheaper to run, but a weaker boundary than a machine.'}
        </Text>

        <Group grow>
          <Switch
            label="Ephemeral"
            description="Take one job, then be replaced by a clean runner"
            checked={values.ephemeral ?? false}
            onChange={(event) => form.setFieldValue('ephemeral', event.currentTarget.checked)}
          />
          <Switch
            label="Nested virtualisation"
            description={isVM ? 'Jobs can boot machines of their own' : 'Passes the host /dev/kvm in'}
            checked={values.nested ?? false}
            onChange={(event) => form.setFieldValue('nested', event.currentTarget.checked)}
          />
        </Group>

        {values.nested && !isVM && (
          <Alert color="orange" variant="light" icon={<IconAlertTriangle size={16} />}>
            A container with nested virtualisation gets the host's KVM device. That is a real hole
            in an already weaker boundary — a virtual machine is the safer place for jobs that need
            it.
          </Alert>
        )}

        <Divider label="Scaling" labelPosition="left" />

        <Group grow align="flex-start">
          <NumberInput
            label="Minimum runners"
            description="Kept up even when nothing is running"
            min={1}
            max={64}
            {...form.getInputProps('minReplicas')}
          />
          <NumberInput
            label="Maximum runners"
            description="Set it equal to the minimum for a fixed size"
            min={1}
            max={64}
            {...form.getInputProps('maxReplicas')}
          />
        </Group>
        <Text size="xs" c="dimmed" mt={-8}>
          {(values.maxReplicas ?? 1) > (values.minReplicas ?? 1)
            ? `The pool sits at ${values.minReplicas} and adds a runner whenever every one of them is busy, up to ${values.maxReplicas}. It returns to ${values.minReplicas} after a few minutes with no work.`
            : `A fixed ${values.minReplicas} runner${(values.minReplicas ?? 1) === 1 ? '' : 's'}: it never grows, however much work arrives.`}
        </Text>

        <Divider label="Size" labelPosition="left" />

        <Group grow>
          <NumberInput label="vCPUs" min={1} max={64} {...form.getInputProps('cpus')} />
          <NumberInput
            label="Memory (MiB)"
            min={512}
            step={512}
            {...form.getInputProps('memoryMb')}
          />
          {isVM && <NumberInput label="Disk (GiB)" min={10} {...form.getInputProps('diskGb')} />}
        </Group>
        <Text size="xs" c="dimmed" mt={-8}>
          Every runner is a machine of this size, so at its maximum the pool wants{' '}
          {((values.maxReplicas ?? 0) * (values.memoryMb ?? 0)) / 1024} GiB of memory at once.
        </Text>

        <Divider label="Labels" labelPosition="left" />

        <TagsInput
          label="Extra labels"
          description="What a workflow targets with runs-on"
          placeholder="gpu, eu-west"
          value={values.labels ?? []}
          onChange={(value) => form.setFieldValue('labels', value)}
        />
        <div>
          <Text size="xs" c="dimmed" mb={4}>
            Runners will register with:
          </Text>
          <Group gap={4}>
            <Badge size="sm" variant="default">
              self-hosted
            </Badge>
            {effectiveLabels(values).map((label) => (
              <Badge key={label} size="sm" variant="dot">
                {label}
              </Badge>
            ))}
          </Group>
        </div>

        <TextInput
          label="Image"
          description="Which image these runners boot. Per-repository images will select one here."
          {...form.getInputProps('image')}
        />

        <Group justify="space-between" mt="sm">
          <Switch
            label="Enabled"
            checked={values.enabled ?? true}
            onChange={(event) => form.setFieldValue('enabled', event.currentTarget.checked)}
          />
          <Group>
            <Button variant="default" onClick={onCancel} type="button">
              Cancel
            </Button>
            <Button type="submit" loading={saving}>
              {pool.id ? 'Save' : 'Create'}
            </Button>
          </Group>
        </Group>

        {pool.id && (
          <Text size="xs" c="dimmed">
            Changing anything but the scaling bounds replaces the pool's runners. They are drained
            first, so no job is lost — a busy runner is replaced when it finishes.
          </Text>
        )}
      </Stack>
    </form>
  )
}
