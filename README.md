# AgentMoor

AgentMoor lets a SaaS application run tasks with native agent CLIs on Kubernetes and keep the conversation going after the Pod is gone. It stores each agent's session, workspace, skills, and output files independently of the Pod that did the work.

For example, your app can ask an agent to total a CSV, download the report, and delete the Pod. If a user later asks how the agent calculated a number, the app sends the question with the same session ID. With follow-up scheduling enabled, AgentMoor starts a new Pod, restores the workspace and native session, and makes the answer available for download.

Codex CLI, Gemini CLI, OpenCode, and Claude Code share one HTTPS API for submitting tasks, checking progress, cancelling work, and downloading a specific version of a result. Each adapter handles its agent's native formats. AgentMoor manages ownership, versions, checksums, and access. [SaaS API and Kubernetes setup](docs/task-contract.md)

**v0.1 technical preview** · [MIT license](LICENSE). The acceptance tests run real agent CLIs against a simulated model endpoint in a local kind cluster. They cover task delivery, cancellation during execution, and two consecutive follow-ups in fresh Pods. If you try your own SaaS workflow with a live model, include the CLI and model versions when sharing results. [Test results](docs/mvp.md) · [Contributing](CONTRIBUTING.md)

For a consumer-side example, see [Import a verified report](examples/report-consumer/README.md).

## Quick start

On macOS or Linux, install Go 1.27.4, a C compiler, Make, and Python 3. Run the local asset recovery demo from the repository root.

```sh
make demo
```

The demo starts a local server, saves one asset of each type, and repeats the sync. It then restores the workspace and compares the restored file with the original. You should see four `v1` records followed by `Workspace restored and verified.` The demo cleans up its temporary directory on exit. If you already have `bin/agentmoor`, you can run `sh examples/demo.sh` directly.

To list the available adapters and inspect the sample Codex workspace, run the commands below. To inspect your own workspace, replace `testdata/codex` with its path.

```sh
./bin/agentmoor adapters
./bin/agentmoor detect testdata/codex
./bin/agentmoor skills codex testdata/codex
```

## What gets saved

| Asset | What AgentMoor saves and how it restores it |
| --- | --- |
| Session | Preserves the native transcript, session ID, and related files the adapter recognizes. File-based agents support bulk session recovery. The OpenCode adapter uses the native CLI to export and import sessions. |
| Skill | Packages the manifest with scripts and reference files from the skill directory. Records the name, declared version, dependencies, and management source. Restores the package to a new path. |
| Artifact | Scans the output directory assigned by the control plane. Each content change produces a new version, and earlier versions remain available for download. |
| Workspace | Snapshots the directory tree, regular files, standard permissions, and supported relative symlinks. Verifies the SHA-256 checksum during restore and reports a conflict if the destination already exists. |

A runtime hook can invoke `sync` once. For continuous sync, `watch` listens for changes and rescans periodically. Each worker receives short-lived credentials scoped to its assignment. By default, AgentMoor uses SQLite and local disk. It also supports PostgreSQL and S3-compatible object storage.

## Development and testing

```sh
make check          # Check formatting, run go vet and tests with the race detector, then build
make demo           # Upload, sync again, and restore assets using the local server
make test-k8s       # Test delivery, cancellation, and cross-Pod follow-ups in a disposable kind cluster
make test-k8s-file-report # Test Codex file tools and edits across fresh Pods
make test-k8s-skills # Test pinned native Skills, their scripts, and cross-Pod loading
make test-services  # Test PostgreSQL and S3-compatible storage with Docker Compose
make test-k8s-persistent # Run Codex file/Skill workflows on PostgreSQL and S3, including API restart
```

To try the Kubernetes workflow with Codex:

```sh
make check-kind-prereqs  # Check host tools and Docker without building or creating a cluster
make test-k8s AGENT=codex
```

The second command also runs the preflight before it builds. The kind test uses a deterministic model endpoint and needs no model API key. It does need network access for first-time image and CLI downloads. See the [prerequisites and expected results](docs/task-contract.md#一条命令复验).

The native CLI recovery tests can also run outside Kubernetes. They require the matching CLI version and start a local model endpoint. Each test creates a conversation, removes the original directories, restores the session, and checks that the next model request contains a random marker from the first turn. [Reproduction steps](docs/operations.md)

The Codex file test uses a scripted model to request a deployment-selected MCP
tool. The tool reads CSV data, writes real report files, and revises them in new
Pods with the same native session. The SaaS client downloads pinned versions,
checks their bytes and amounts, and imports reports into its own SQLite database.
It also checks database failure retries and cancellation during a tool call.
No model account or API key is needed. [File task contract](docs/task-contract.md#文件交付和多份产物)

`make test-k8s-skills` adds a versioned Skill package to that workflow. It checks
native Skill body loading, execution of the restored package's script and
reference file, and retention of the selected version after a newer version is
published. It also verifies that the same input fails without the required Skill.

## Project documentation

The linked guides are in Chinese.

| Guide | What's inside |
| --- | --- |
| [Operations guide](docs/operations.md) | Start the platform, configure workers, restore sessions, and resolve version conflicts. |
| [Asset lifecycle](docs/asset-lifecycle.md) | Creation, updates, local deletion, fixed references, recovery, and garbage collection. |
| [v0.1 acceptance results](docs/mvp.md) | Tested CLI versions, product requirements, test coverage, and known limits. |
| [Adapter research](docs/research.md) | Design references from Magpie, CC Switch, and the four supported agents. |
| [Repository guide](AGENTS.md) | Code layout, adapter conventions, and checks to run before submitting a change. |
| [Workspace upload baseline](docs/workspace-upload-baseline.md) | Measure actual upload bytes for large synthetic workspaces and check fixed-version recovery. |
| [Original product requirements](docs/PRD-v0.1.md) | Archived first draft. The acceptance results list the agents included in this preview. |
