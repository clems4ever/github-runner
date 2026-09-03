import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MantineProvider } from "@mantine/core";
import { describe, expect, it, vi } from "vitest";
import { PoolEditor } from "./PoolEditor";
import { api, ApiError, emptyPool } from "../api";

function renderEditor(pool = emptyPool(1)) {
  return render(
    <MantineProvider>
      <PoolEditor
        pool={pool}
        credentials={[
          { id: 1, name: "pat", kind: "pat", hint: "…1234", createdAt: "" },
        ]}
        onSaved={vi.fn()}
        onCancel={vi.fn()}
      />
    </MantineProvider>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("PoolEditor", () => {
  it("shows the labels the runners will register with, before saving", async () => {
    renderEditor();
    expect(screen.getByText("self-hosted")).toBeInTheDocument();
    expect(screen.getByText("vm")).toBeInTheDocument();

    await userEvent.click(
      screen.getByRole("switch", { name: /Nested virtualisation/ }),
    );
    expect(screen.getByText("nestedvirt")).toBeInTheDocument();
  });

  it("warns that a container with nested virtualisation is a weaker boundary", async () => {
    renderEditor();
    expect(
      screen.queryByText(/hole in an already weaker boundary/),
    ).not.toBeInTheDocument();

    await userEvent.click(screen.getByText("Container"));
    await userEvent.click(
      screen.getByRole("switch", { name: /Nested virtualisation/ }),
    );

    expect(
      screen.getByText(/hole in an already weaker boundary/),
    ).toBeInTheDocument();
  });

  it("hides the disk size for containers, which have none", async () => {
    renderEditor();
    expect(
      screen.getByRole("textbox", { name: "Disk (GiB)" }),
    ).toBeInTheDocument();
    await userEvent.click(screen.getByText("Container"));
    expect(
      screen.queryByRole("textbox", { name: "Disk (GiB)" }),
    ).not.toBeInTheDocument();
  });

  it("refuses a repository that is not owner/name", async () => {
    renderEditor();
    await userEvent.type(screen.getByRole("textbox", { name: /^Name/ }), "web");
    await userEvent.type(
      screen.getByPlaceholderText("owner/repository"),
      "runyard",
    );
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    expect(
      await screen.findByText("A repository is owner/name"),
    ).toBeInTheDocument();
  });

  // A layer is an overlay on a machine's disk and belongs to one repository,
  // so the control has no meaning on a container or on an organisation pool.
  // Offering it there would be offering a setting the daemon then refuses.
  it("offers repository definitions only where they can work", async () => {
    renderEditor();
    expect(
      screen.getByText(/Let the repository add to this image/),
    ).toBeInTheDocument();

    await userEvent.click(screen.getByText("Container"));
    expect(
      screen.queryByText(/Let the repository add to this image/),
    ).not.toBeInTheDocument();

    await userEvent.click(screen.getByText("Virtual machine"));
    await userEvent.click(screen.getByText("Organisation"));
    expect(
      screen.queryByText(/Let the repository add to this image/),
    ).not.toBeInTheDocument();
  });

  // Trusting a repository unread is a real thing to want and a large thing to
  // do by accident, so the consequence is spelled out where it is chosen.
  it("spells out what trusting a repository unread means", async () => {
    renderEditor({ ...emptyPool(1), layers: "trust" });
    expect(screen.getByText(/run a script as root/)).toBeInTheDocument();

    renderEditor({ ...emptyPool(1), layers: "approve" });
    expect(screen.getByText(/waits on the Layers page/)).toBeInTheDocument();
  });

  // A pool switched to a container keeps whatever the layer control was left
  // at, and the daemon refuses to save it — with a message about a field that
  // is no longer on the screen.
  it("drops the layer policy when the pool can no longer have one", async () => {
    const createPool = vi
      .spyOn(api, "createPool")
      .mockResolvedValue({} as never);

    renderEditor({
      ...emptyPool(1),
      name: "web",
      scope: "o/r",
      layers: "approve",
    });
    await userEvent.click(screen.getByText("Container"));
    await userEvent.click(screen.getByRole("button", { name: "Create" }));

    expect(createPool).toHaveBeenCalledWith(
      expect.objectContaining({ layers: "off" }),
    );
  });

  it("says what the pool will cost in memory at its maximum", async () => {
    renderEditor({
      ...emptyPool(1),
      minReplicas: 1,
      maxReplicas: 3,
      memoryMb: 4096,
    });
    expect(screen.getByText(/12 GiB of memory at once/)).toBeInTheDocument();
  });
});

describe("when GitHub refuses the pool", () => {
  // An app cannot widen its own access — GitHub does not allow it — so the most
  // the daemon can do is say what is wrong and put the page that fixes it one
  // click away.
  it("shows the reason on the form, with a way to fix it", async () => {
    vi.spyOn(api, "createPool").mockRejectedValue(
      new ApiError(
        "GitHub returned 404 for clems4ever/claude-control — the app is authenticated but has no installation covering it",
        400,
        "https://github.com/settings/installations/42",
      ),
    );

    renderEditor();
    await userEvent.type(screen.getByRole("textbox", { name: /^Name/ }), "web");
    await userEvent.type(
      screen.getByPlaceholderText("owner/repository"),
      "clems4ever/claude-control",
    );
    await userEvent.click(screen.getByRole("button", { name: "Create" }));

    expect(
      await screen.findByText(/no installation covering it/),
    ).toBeInTheDocument();
    const grant = screen.getByRole("link", { name: /Grant access on GitHub/ });
    expect(grant).toHaveAttribute(
      "href",
      "https://github.com/settings/installations/42",
    );
  });

  // A token has no installation page, so there is nothing to link to and the
  // message stands alone.
  it("offers no link when there is nowhere to send anyone", async () => {
    vi.spyOn(api, "createPool").mockRejectedValue(
      new ApiError("GitHub returned 403", 400),
    );

    renderEditor();
    await userEvent.type(screen.getByRole("textbox", { name: /^Name/ }), "web");
    await userEvent.type(
      screen.getByPlaceholderText("owner/repository"),
      "o/r",
    );
    await userEvent.click(screen.getByRole("button", { name: "Create" }));

    expect(await screen.findByText(/GitHub returned 403/)).toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: /Grant access/ }),
    ).not.toBeInTheDocument();
  });
});
