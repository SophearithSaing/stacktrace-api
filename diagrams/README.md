# Diagram index

| File                                                           | Diagram                   | Contents                                                                 |
| -------------------------------------------------------------- | ------------------------- | ------------------------------------------------------------------------ |
| [`infrastructure.mmd`](infrastructure.mmd)                     | Mermaid flowchart         | Deployment, process, package, database, and external-provider boundaries |
| [`endpoint-flows.md`](endpoint-flows.md)                       | Mermaid sequence diagrams | Shared HTTP pipeline and every public API endpoint family                |
| [`generation-trigger-flow.mmd`](generation-trigger-flow.mmd)   | Mermaid sequence diagram  | Social and scheduled triggers through worker execution and publication   |
| [`generation-job-lifecycle.mmd`](generation-job-lifecycle.mmd) | Mermaid state diagram     | Generation job states, claims, retries, cleanup, and terminal outcomes   |
| [`identity-content.mmd`](identity-content.mmd)                 | Mermaid ER diagram        | Identity, authentication, and social-content tables                      |
| [`generation.mmd`](generation.mmd)                             | Mermaid ER diagram        | Generation settings, jobs, attempts, and publication relationships       |
| [`data-model.dbml`](data-model.dbml)                           | DBML schema               | Complete physical PostgreSQL data model                                  |

## Optional verification

Node 26+ and npm are optional development tooling, not requirements for Go builds.
Run `make diagrams-setup` once (and after `tools/diagrams/package-lock.json` changes),
then `make diagrams-check`; focused checks are `npm --prefix tools/diagrams test`.
The locked dependencies are reused from npm's normal cache. Update them deliberately
with npm, commit both package files, then rerun the checks.

The checker parses Mermaid and DBML and compares the endpoint index with explicit
ServeMux registrations. It does not prove diagram semantics, SQL/migration accuracy,
or rendered layout; migrations and source remain authoritative. Render diagrams only
when reviewing layout.
