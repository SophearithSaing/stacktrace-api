import { readFile, readdir } from "node:fs/promises";
import { existsSync } from "node:fs";
import path from "node:path";
import process from "node:process";

const root = path.resolve(import.meta.dirname, "../..");

function nodeVersion() {
  return Number(process.versions.node.split(".")[0]);
}

async function setupDOM() {
  const { JSDOM } = await import("jsdom");
  const dom = new JSDOM("<body></body>");
  globalThis.window = dom.window;
  globalThis.document = dom.window.document;
}

async function mermaidParser() {
  await setupDOM();
  const { default: mermaid } = await import("mermaid");
  mermaid.initialize({ startOnLoad: false });
  return mermaid;
}

async function files(directory) {
  const entries = await readdir(directory, { withFileTypes: true });
  const result = [];
  for (const entry of entries.sort((left, right) =>
    left.name.localeCompare(right.name),
  )) {
    const target = path.join(directory, entry.name);
    if (entry.isDirectory()) result.push(...(await files(target)));
    else result.push(target);
  }
  return result;
}

function mermaidBlocks(source, filename) {
  const blocks = [];
  const lines = source.split(/\r?\n/);
  let open = null;
  for (let index = 0; index < lines.length; index++) {
    if (open === null && /^```mermaid\s*$/.test(lines[index])) open = index + 1;
    else if (open !== null && /^```\s*$/.test(lines[index])) {
      blocks.push({
        label: `${filename}#${blocks.length + 1}`,
        source: lines.slice(open, index).join("\n"),
      });
      open = null;
    }
  }
  if (open !== null)
    throw new Error(`${filename}:${open}: unclosed mermaid fence`);
  return blocks;
}

function routeIndexSection(markdown) {
  const lines = markdown.split(/\r?\n/);
  let inSection = false;
  const section = [];
  for (const line of lines) {
    if (/^## Route index\s*$/.test(line)) {
      inSection = true;
      continue;
    }
    if (inSection && /^## /.test(line)) break;
    if (inSection) section.push(line);
  }
  return section.join("\n");
}

function indexedEndpoints(markdown) {
  const endpoints = new Set();
  for (const match of routeIndexSection(markdown).matchAll(
    /`([A-Z]+ \/[^`]+)`/g,
  )) {
    endpoints.add(match[1]);
  }
  return endpoints;
}

function registeredEndpoints(server) {
  const endpoints = new Set();
  for (const match of server.matchAll(
    /s\.mux\.HandleFunc\("([A-Z]+) ([^"]+)"/g,
  )) {
    const endpoint = `${match[1]} ${match[2]}`;
    // ServeMux registers the constrained wildcard as {resource} so by-handle/{handle}
    // wins, but the handler only accepts the feed resource; the index documents
    // the effective route as {accountID}/feed.
    endpoints.add(
      endpoint === "GET /api/v1/accounts/{accountID}/{resource}"
        ? "GET /api/v1/accounts/{accountID}/feed"
        : endpoint,
    );
  }
  return endpoints;
}

export async function checkDiagrams({
  diagrams = path.join(root, "diagrams"),
  server = path.join(root, "internal/api/server.go"),
} = {}) {
  if (nodeVersion() < 26)
    throw new Error(
      `Node ${process.versions.node} is unsupported; use Node 26+`,
    );
  const parser = await mermaidParser();
  const candidates = (await files(diagrams)).filter((file) =>
    /\.(mmd|md|dbml)$/.test(file),
  );
  let mermaidCount = 0;
  let dbmlCount = 0;
  for (const file of candidates) {
    const source = await readFile(file, "utf8");
    if (file.endsWith(".mmd")) {
      if (source.trim() === "")
        throw new Error(`${file}: empty Mermaid source`);
      try {
        await parser.parse(source);
      } catch (error) {
        throw new Error(`${file}: ${error.message}`);
      }
      mermaidCount++;
    } else if (file.endsWith(".md")) {
      for (const block of mermaidBlocks(source, file)) {
        try {
          await parser.parse(block.source);
        } catch (error) {
          throw new Error(`${block.label}: ${error.message}`);
        }
        mermaidCount++;
      }
    } else {
      const { Parser } = await import("@dbml/core");
      try {
        Parser.parse(source, "dbml");
      } catch (error) {
        throw new Error(`${file}: ${error.message}`);
      }
      dbmlCount++;
    }
  }
  const index = indexedEndpoints(
    await readFile(path.join(diagrams, "endpoint-flows.md"), "utf8"),
  );
  const routes = registeredEndpoints(await readFile(server, "utf8"));
  const missing = [...routes].filter((route) => !index.has(route));
  const stale = [...index].filter((route) => !routes.has(route));
  if (missing.length || stale.length)
    throw new Error(
      `route index mismatch: missing [${missing.join(", ")}], stale [${stale.join(", ")}]`,
    );
  return { mermaidCount, dbmlCount, routes: routes.size };
}

if (import.meta.main) {
  if (!existsSync(path.join(import.meta.dirname, "node_modules"))) {
    console.error(
      "Diagram tooling dependencies are missing; run make diagrams-setup (Node 26+).",
    );
    process.exitCode = 1;
  } else {
    try {
      const result = await checkDiagrams();
      console.log(
        `PASS diagrams: ${result.mermaidCount} Mermaid, ${result.dbmlCount} DBML, ${result.routes} routes`,
      );
    } catch (error) {
      console.error(`Diagram check failed: ${error.message}`);
      process.exitCode = 1;
    }
  }
}
