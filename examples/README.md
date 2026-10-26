# Examples

`feedback.example.json` is hand-written synthetic data that validates against the published feedback revision schema. It is a `schemaVersion` `0.1.0` revision with a `page-viewport` source, only rectangle, pin and redaction annotations, and a description with expected and actual results. The schema still accepts this older shape, so the example stays as it is. Image sizes, hashes, targets, timestamps and browser version fields are contract-test values. They do not mean a real page was captured, and no matching PNG exists.

A `drawing` source, ellipse, diamond or line annotations, or a task with no description (purely visual feedback) need `0.2.0`. `tools/validate_contracts.py` derives and tests those variants from this example instead of storing them as separate files.

`task-template.md` is a task form you can fill in by hand. Every section is optional; the product itself requires no description or expected and actual results (purely visual feedback is valid from schema `0.2.0`). In a real share bundle, the Gateway generates `task.md` at publication, puts the human-written fields in code fences and labels them untrusted data. Do not use the template instead of it. A real bundle must pass image redaction, hash and size checks, access control and the publication flow. Never use this example's human approval fields in place of a real human confirmation.

The UI shows one primary status: Sent (queued or read, not started), In progress, Done (the Agent reports a result; a human has not necessarily verified it) or Cancelled. The exact delivery state is shown beside it and is the authority.
