# Generate a report from a CSV file

Run these commands from the repository root on macOS or Linux with Python 3.
The example uses synthetic sales in integer cents. The tool reads the CSV and
writes a new JSON file compatible with the [report consumer](../report-consumer/README.md).

```sh
report_dir=$(mktemp -d)
python3 examples/file-report/summarize.py examples/file-report/sales.csv "$report_dir/report.json"
cat "$report_dir/report.json"
shasum -a 256 "$report_dir/report.json"
```

East's sale of 105 cents and refund of 7 cents sum to 98. West has one sale of
31 cents. The result is 39 bytes including its final newline.

```json
{"totals_cents":{"east":98,"west":31}}
```

The expected SHA-256 is `f72401084eda1ff431cf23833d909e242747c3ac0a641e90abf16db6183c9836`.
The destination directory must exist. On success the tool links a fully
written file into place. Missing input, malformed rows and an existing
destination produce a nonzero exit. A prior report keeps its original bytes.
The output remains in `$report_dir` for inspection.

## Verify the tool and the native Codex call

```sh
make test-file-report
python3 tests/codex_file_report.py --codex /absolute/path/to/codex -v
```

The first command runs the tool as a Python subprocess. The second runs the
same four cases through the supplied native CLI in temporary workspaces. It
starts a scripted Responses endpoint on loopback and gives Codex a fresh home.
The endpoint requests one fixed shell command and reads the tool status. Only
the tool opens the CSV and creates the report. The native test checks the
matching tool response and the command's actual exit code. The final assistant
message contains only `done` or `failed`.

The success case adds a newly generated sale to the fixture before invoking the
tool. The verifier computes the expected cents independently, checks every byte
from disk and prints the SHA-256. The three error cases check missing input,
a malformed amount after a valid row, and a pre-existing output. Each test
removes its temporary directory on exit.

This local example covers file generation. Versioned publication by an
AgentMoor Worker, retrieval by a SaaS client, cross-Pod continuation and live
model behavior have separate acceptance criteria in the
[verification matrix](../../docs/verification-matrix.md).
