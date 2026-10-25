# Specification validation report

Validation date: 2026-10-25

| Check | Result |
|---|---|
| Test cases | 33 |
| Passed | 33 |
| Failures / errors | 0 |
| Skipped | 0 |
| Core API operations | 28 |

This run checks JSON Schema definitions, synthetic feedback examples, drawing and annotation constraints, negative inputs, local OpenAPI references and structure, 500 coordinate round trips, font configuration XML and fixture JavaScript syntax. Example hashes do not correspond to real image files.

The validator starts no container or browser. Its result does not establish Runtime, Gateway, UI, Agent image-reading, persistence, update or rollback behavior. It does not run a full external OpenAPI validator.

Actual integrated-product evidence and remaining limits are recorded separately in [the acceptance matrix](ACCEPTANCE.md#evidence-status) and [the update implementation guide](UPDATE-IMPLEMENTATION.md#exercised-evidence-and-limits). Historical M0 results are in [the spike reports](spikes/). These records are maintained independently and are not overwritten by this generator.

Regenerate this file with `mise exec -- uv run --script tools/validate_spec.py --report docs/VALIDATION-REPORT.md --date 2026-10-25`. `mise run validate` and `mise run check` run checks without writing this report.
