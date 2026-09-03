import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MantineProvider } from "@mantine/core";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { LayersPage } from "./LayersPage";
import { api, type Pool, type RepoLayer } from "../api";

vi.mock("../api", async () => {
  const actual = await vi.importActual<typeof import("../api")>("../api");
  return {
    ...actual,
    api: { ...actual.api, layers: vi.fn(), decideLayer: vi.fn() },
  };
});

const layers = vi.mocked(api.layers);
const decideLayer = vi.mocked(api.decideLayer);

const layer = (over: Partial<RepoLayer> = {}): RepoLayer => ({
  id: 1,
  pool: "web",
  repo: "clems4ever/runyard",
  digest: "abcdef0123456789",
  packages: ["ffmpeg", "imagemagick"],
  recipe:
    "#!/usr/bin/env bash\ncurl -fsSL https://example.test/toolchain | tar -C /usr/local -xz\n",
  image: "",
  approval: "pending",
  decidedBy: "",
  firstSeen: "2026-08-28T12:00:00Z",
  lastSeen: "2026-08-28T12:00:00Z",
  decidedAt: "",
  ...over,
});

const pool = (over: Partial<Pool> = {}) =>
  ({
    id: 1,
    name: "web",
    scope: "clems4ever/runyard",
    layers: "approve",
    ...over,
  }) as Pool;

async function show(pools: Pool[] = [pool()]) {
  const result = render(
    <MantineProvider>
      <LayersPage pools={pools} />
    </MantineProvider>,
  );
  // The list is fetched on mount; nothing on the page is true until it lands.
  await waitFor(() => expect(layers).toHaveBeenCalled());
  return result;
}

beforeEach(() => {
  vi.clearAllMocks();
  layers.mockResolvedValue([]);
});

describe("LayersPage", () => {
  // The decision is about a script, so the script is on the page. An approval
  // button next to a package count is a click, not a decision.
  it("shows what the repository is asking to run", async () => {
    layers.mockResolvedValue([layer()]);
    await show();

    expect(await screen.findByText("clems4ever/runyard")).toBeInTheDocument();
    expect(screen.getByText("ffmpeg")).toBeInTheDocument();
    expect(
      screen.getByText(/toolchain \| tar -C \/usr\/local/),
    ).toBeInTheDocument();
  });

  it("approves the definition that was on screen, not just the row", async () => {
    layers.mockResolvedValue([layer()]);
    decideLayer.mockResolvedValue(
      layer({ approval: "approved", decidedBy: "admin" }),
    );
    await show();

    await userEvent.click(
      await screen.findByRole("button", { name: /Approve/ }),
    );

    // The digest goes with the decision: it is what makes an approval refer to
    // a definition rather than to a row that may have changed underneath.
    expect(decideLayer).toHaveBeenCalledWith(1, "approved", "abcdef0123456789");
  });

  it("refuses", async () => {
    layers.mockResolvedValue([layer()]);
    decideLayer.mockResolvedValue(layer({ approval: "refused" }));
    await show();

    await userEvent.click(
      await screen.findByRole("button", { name: /Refuse/ }),
    );
    expect(decideLayer).toHaveBeenCalledWith(1, "refused", "abcdef0123456789");
  });

  // An approval is reversible. Somebody who approved a thing and then read it
  // properly needs a way back that is not editing the database.
  it("lets an approval be withdrawn", async () => {
    layers.mockResolvedValue([
      layer({ approval: "approved", decidedBy: "admin" }),
    ]);
    decideLayer.mockResolvedValue(layer({ approval: "refused" }));
    await show();

    await userEvent.click(
      await screen.findByRole("button", { name: /Withdraw approval/ }),
    );
    expect(decideLayer).toHaveBeenCalledWith(1, "refused", "abcdef0123456789");
  });

  // The only thing on this page anybody has to do is the undecided one, and a
  // host that has been running for a while has far more decided rows than
  // waiting ones.
  it("puts what is waiting above what is settled", async () => {
    layers.mockResolvedValue([
      layer({
        id: 1,
        repo: "clems4ever/settled",
        approval: "approved",
        image: "runner-x.qcow2",
      }),
      layer({ id: 2, repo: "clems4ever/waiting", digest: "ffff" }),
    ]);
    await show();

    const repos = await screen.findAllByText(/clems4ever\//);
    expect(repos[0]).toHaveTextContent("clems4ever/waiting");
  });

  it("counts what is waiting where it can be seen without reading the list", async () => {
    layers.mockResolvedValue([layer(), layer({ id: 2, digest: "ffff" })]);
    await show();
    expect(
      await screen.findByText("2 waiting for a decision"),
    ).toBeInTheDocument();
  });

  // A built layer is a different thing from an approved one — the build takes
  // minutes and can fail — and the page that says "approved" while nothing has
  // been built is the page somebody stares at wondering why the job still
  // cannot find ffmpeg.
  it("distinguishes approved from built", async () => {
    layers.mockResolvedValue([
      layer({ approval: "approved", decidedBy: "admin" }),
    ]);
    await show();
    expect(await screen.findByText("approved")).toBeInTheDocument();
    expect(screen.getByText(/build is queued/)).toBeInTheDocument();

    layers.mockResolvedValue([
      layer({
        approval: "approved",
        decidedBy: "admin",
        image: "runner-noble-web-abc.qcow2",
      }),
    ]);
    await show();
    expect(await screen.findByText("built")).toBeInTheDocument();
  });

  // A row that says "approved" and nothing else invites the question this
  // answers: a pool on trust was never read by a person, and that is a
  // different fact from an operator having approved it.
  it("says when nobody read it", async () => {
    layers.mockResolvedValue([
      layer({ approval: "approved", decidedBy: "policy" }),
    ]);
    await show();
    expect(await screen.findByText(/Decided by policy/)).toBeInTheDocument();
  });

  // An empty page on a fleet where no pool reads definitions is not a bug
  // report, it is a feature nobody has switched on.
  it("explains itself on a fleet that uses none", async () => {
    await show([pool({ layers: "off" })]);
    expect(
      await screen.findByText(/\.github\/runner-fleet\.yml/),
    ).toBeInTheDocument();
    expect(screen.getByText(/turn it on/)).toBeInTheDocument();
  });

  // With a pool that is reading, an empty list means the file is not there —
  // which is a different thing to say, and the one that sends somebody to the
  // right place.
  it("says the file is missing when a pool is reading and nothing came back", async () => {
    await show();
    expect(
      await screen.findByText("Nothing has been asked for yet"),
    ).toBeInTheDocument();
    expect(screen.getByText(/on the default branch, or nothing in it/)).toBeInTheDocument();
  });
});
