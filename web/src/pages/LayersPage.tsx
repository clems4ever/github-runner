import { useCallback, useEffect, useMemo, useState } from "react";
import {
  Alert,
  Badge,
  Button,
  Card,
  Center,
  Code,
  Collapse,
  Group,
  Loader,
  Stack,
  Text,
  Title,
  Tooltip,
} from "@mantine/core";
import { notifications } from "@mantine/notifications";
import {
  IconChevronDown,
  IconChevronRight,
  IconCircleCheck,
  IconCircleX,
  IconStack3,
} from "@tabler/icons-react";
import { api, type Pool, type RepoLayer } from "../api";

/**
 * What repositories have asked their pools for.
 *
 * The page exists for one action, taken rarely: an operator reads a package
 * list and a shell script that somebody merged into a repository, and says
 * whether this host will run it. Everything else here is in service of that
 * being a decision rather than a click — which is why the recipe is on the page
 * in full rather than behind a link, and why nothing is approved in bulk.
 */
export function LayersPage({ pools }: { pools: Pool[] }) {
  const [layers, setLayers] = useState<RepoLayer[] | null>(null);

  const refresh = useCallback(async () => {
    try {
      setLayers(await api.layers());
    } catch (error) {
      notifications.show({
        color: "red",
        title: "Could not read the layers",
        message: error instanceof Error ? error.message : String(error),
      });
    }
  }, []);

  useEffect(() => {
    void refresh();
    // A repository can publish a definition at any moment and the daemon finds
    // it on its own schedule, so a page left open should show it arriving.
    const timer = setInterval(() => void refresh(), 10000);
    return () => clearInterval(timer);
  }, [refresh]);

  // Waiting first, and only then by how recently they were seen. A decision
  // nobody has made is the only thing on this page anybody has to do.
  const sorted = useMemo(() => {
    const rank = (layer: RepoLayer) => (layer.approval === "pending" ? 0 : 1);
    return [...(layers ?? [])].sort((a, b) => rank(a) - rank(b));
  }, [layers]);

  const enabled = pools.filter((pool) => pool.layers && pool.layers !== "off");
  const waiting = sorted.filter((layer) => layer.approval === "pending").length;

  if (layers === null) {
    return (
      <Center py="xl">
        <Loader />
      </Center>
    );
  }

  return (
    <Stack gap="lg">
      <Group justify="space-between" gap="sm" wrap="wrap">
        <Title order={3}>Layers</Title>
        {waiting > 0 && (
          <Badge color="yellow" variant="light" size="lg">
            {waiting} waiting for a decision
          </Badge>
        )}
      </Group>

      {enabled.length === 0 && (
        <Alert variant="light" color="blue">
          A repository can keep what its jobs need next to its jobs, in{" "}
          <Code>.github/runner-fleet.yml</Code> on its default branch, and the
          daemon builds it a layer on top of its pool&rsquo;s image — so the
          packages are baked in once instead of installed by every job. No pool
          is reading one yet: turn it on for a repository-scoped machine pool in
          the pool editor.
        </Alert>
      )}

      {layers.length === 0 && enabled.length > 0 && (
        <Card withBorder padding="xl">
          <Center>
            <Stack align="center" gap="xs">
              <IconStack3 size={32} stroke={1.2} opacity={0.5} />
              <Text fw={500}>Nothing has been asked for yet</Text>
              <Text size="sm" c="dimmed" ta="center">
                {enabled.map((pool) => pool.scope).join(", ")} — no{" "}
                <Code>.github/runner-fleet.yml</Code> on the default branch, or
                nothing in it.
              </Text>
            </Stack>
          </Center>
        </Card>
      )}

      {sorted.map((layer) => (
        <LayerCard key={layer.id} layer={layer} onDecided={refresh} />
      ))}
    </Stack>
  );
}

/** One definition, and the decision about it. */
function LayerCard({
  layer,
  onDecided,
}: {
  layer: RepoLayer;
  onDecided: () => Promise<void>;
}) {
  // The recipe is what is being decided, so it is open by default on anything
  // undecided and folded away once it is not a question any more.
  const [open, setOpen] = useState(layer.approval === "pending");
  const [deciding, setDeciding] = useState<"approved" | "refused" | null>(null);

  const decide = async (approval: "approved" | "refused") => {
    setDeciding(approval);
    try {
      await api.decideLayer(layer.id, approval, layer.digest);
      await onDecided();
    } catch (error) {
      notifications.show({
        color: "red",
        title: "The decision was not taken",
        message: error instanceof Error ? error.message : String(error),
      });
    } finally {
      setDeciding(null);
    }
  };

  return (
    <Card withBorder padding="md">
      <Stack gap="sm">
        <Group justify="space-between" gap="sm" wrap="wrap" align="flex-start">
          <Stack gap={2} style={{ minWidth: 0 }}>
            <Group gap="xs" wrap="nowrap">
              <Text fw={600} style={{ overflowWrap: "anywhere" }}>
                {layer.repo}
              </Text>
              <Approval layer={layer} />
            </Group>
            <Text size="xs" c="dimmed">
              for the <b>{layer.pool}</b> pool ·{" "}
              <Tooltip label={layer.digest}>
                {/* Enough of the digest to tell two definitions apart, which is
                    all anyone ever does with it by eye. */}
                <span>{layer.digest.slice(0, 12)}</span>
              </Tooltip>
            </Text>
          </Stack>

          {layer.approval === "pending" ? (
            <Group gap="xs" wrap="nowrap">
              <Button
                variant="default"
                leftSection={<IconCircleX size={16} />}
                loading={deciding === "refused"}
                onClick={() => void decide("refused")}
              >
                Refuse
              </Button>
              <Button
                leftSection={<IconCircleCheck size={16} />}
                loading={deciding === "approved"}
                onClick={() => void decide("approved")}
              >
                Approve
              </Button>
            </Group>
          ) : (
            // A decision is reversible, and saying so out loud matters more
            // than the button being prominent: an operator who approved
            // something and then read it properly needs a way back.
            <Button
              variant="subtle"
              size="compact-sm"
              loading={deciding !== null}
              onClick={() =>
                void decide(
                  layer.approval === "approved" ? "refused" : "approved",
                )
              }
            >
              {layer.approval === "approved"
                ? "Withdraw approval"
                : "Approve after all"}
            </Button>
          )}
        </Group>

        {layer.packages.length > 0 && (
          <Group gap={4}>
            {layer.packages.map((name) => (
              <Badge key={name} size="sm" variant="default">
                {name}
              </Badge>
            ))}
          </Group>
        )}

        {layer.recipe && (
          <>
            <Group
              gap={4}
              style={{ cursor: "pointer" }}
              onClick={() => setOpen((was) => !was)}
              role="button"
              aria-expanded={open}
            >
              {open ? (
                <IconChevronDown size={14} />
              ) : (
                <IconChevronRight size={14} />
              )}
              <Text size="sm" fw={500}>
                Recipe
              </Text>
              <Text size="xs" c="dimmed">
                runs as root while the layer is built
              </Text>
            </Group>
            <Collapse expanded={open}>
              <Code block style={{ maxHeight: 360, overflow: "auto" }}>
                {layer.recipe}
              </Code>
            </Collapse>
          </>
        )}

        <Text size="xs" c="dimmed">
          {layer.approval === "approved" && layer.image
            ? `Built as ${layer.image}.`
            : layer.approval === "approved"
              ? "Approved — the build is queued behind whatever else this host is building."
              : layer.approval === "refused"
                ? "Refused. The pool keeps running on its own image, and this will not be asked again unless the repository changes it."
                : "Until this is decided the pool keeps running on its own image."}
          {layer.decidedBy && ` Decided by ${layer.decidedBy}.`}
        </Text>
      </Stack>
    </Card>
  );
}

function Approval({ layer }: { layer: RepoLayer }) {
  if (layer.approval === "approved") {
    return (
      <Badge color={layer.image ? "green" : "blue"} variant="light">
        {layer.image ? "built" : "approved"}
      </Badge>
    );
  }
  if (layer.approval === "refused") {
    return (
      <Badge color="gray" variant="light">
        refused
      </Badge>
    );
  }
  return (
    <Badge color="yellow" variant="light">
      waiting
    </Badge>
  );
}
