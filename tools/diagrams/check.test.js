import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";
import { checkDiagrams } from "./check.js";

async function fixture(t, files) {
  const directory = await mkdtemp(path.join(tmpdir(), "diagram-check-"));
  t.after(async () => {
    try {
      await rm(directory, { recursive: true, force: true });
    } catch {
      // Ignore cleanup failures; the OS will reclaim the temp directory.
    }
  });
  for (const [name, contents] of Object.entries(files)) await writeFile(path.join(directory, name), contents);
  return directory;
}

function index(endpoint) {
  return `# Endpoint flows\n\n## Route index\n\n| Endpoints |\n| --- |\n${(Array.isArray(endpoint) ? endpoint : [endpoint]).map((item) => `| \`${item}\` |`).join("\n")}\n`;
}

test("checks valid files and the account-feed route exception", async (t) => {
  const directory = await fixture(t, {
    "endpoint-flows.md": index("GET /api/v1/accounts/{accountID}/feed") + "\n```mermaid\nflowchart TD\nA-->B\n```\n",
    "model.dbml": "Table accounts {\n  id uuid [pk]\n}\n",
    "flow.mmd": "flowchart TD\nA-->B\n",
    "server.go": 's.mux.HandleFunc("GET /api/v1/accounts/{accountID}/{resource}", s.listAccountFeed)\n',
  });
  const result = await checkDiagrams({ diagrams: directory, server: path.join(directory, "server.go") });
  assert.deepEqual(result, { mermaidCount: 2, dbmlCount: 1, routes: 1 });
});

test("ignores routes mentioned outside the route index section", async (t) => {
  const directory = await fixture(t, {
    "endpoint-flows.md": `# Endpoint flows\n\nMention \`GET /outside\` in prose.\n\n${index("GET /registered")}`,
    "server.go": 's.mux.HandleFunc("GET /registered", handler)\ns.mux.HandleFunc("GET /outside", handler)\n',
  });
  await assert.rejects(checkDiagrams({ diagrams: directory, server: path.join(directory, "server.go") }), /route index mismatch/);
});

test("reports malformed Mermaid sources, fenced blocks, and fences", async (t) => {
  for (const [name, source, message] of [
    ["flow.mmd", "flowchart TD\nA-->", /flow\.mmd: Parse error/],
    ["endpoint-flows.md", index("GET /healthz") + "```mermaid\nflowchart TD\nA-->\n```\n", /endpoint-flows\.md#1: Parse error/],
    ["endpoint-flows.md", index("GET /healthz") + "```mermaid\nflowchart TD\nA-->B\n", /unclosed mermaid fence/],
  ]) {
    const directory = await fixture(t, { [name]: source, ...(name === "flow.mmd" ? { "endpoint-flows.md": index("GET /healthz") } : {}), "server.go": "" });
    await assert.rejects(checkDiagrams({ diagrams: directory, server: path.join(directory, "server.go") }), message);
  }
});

test("reports invalid DBML and route index drift", async (t) => {
  let directory = await fixture(t, { "endpoint-flows.md": index("GET /healthz"), "bad.dbml": "Table {", "server.go": "" });
  await assert.rejects(checkDiagrams({ diagrams: directory, server: path.join(directory, "server.go") }));
  directory = await fixture(t, { "endpoint-flows.md": index("GET /stale"), "server.go": 's.mux.HandleFunc("GET /live", handler)\n' });
  await assert.rejects(checkDiagrams({ diagrams: directory, server: path.join(directory, "server.go") }), /route index mismatch/);
});

test("prints a missing dependency diagnostic without touching installed tools", async (t) => {
  const source = await readFile(new URL("./check.js", import.meta.url), "utf8");
  const directory = await mkdtemp(path.join(tmpdir(), "diagram-missing-"));
  t.after(async () => {
    try {
      await rm(directory, { recursive: true, force: true });
    } catch {
      // Ignore cleanup failures; the OS will reclaim the temp directory.
    }
  });
  await writeFile(path.join(directory, "check.js"), source);
  await writeFile(path.join(directory, "package.json"), '{"type":"module"}\n');
  const result = spawnSync(process.execPath, ["check.js"], { cwd: directory, encoding: "utf8" });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /make diagrams-setup/);
});
