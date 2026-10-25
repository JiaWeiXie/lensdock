# Issue / task handoff

**Feedback ID:** `<feedback-id>`  **Revision:** `<revision>`

**Workspace:** `<workspace-id>`  **Image source:** `<desktop | page-viewport | drawing>` (`drawing` is an explanatory image on a blank canvas and has no browser page)

## What to do

<Analyze only, help reproduce, or change and verify, plus the operations allowed this time.>

## Problem description

<Name the numbered or boxed region and what is currently visible.>

## Expected result

<Describe the correct behavior or screen, not just "fix it".>

## Actual result

<Describe the wrong behavior or screen and whether it reproduces reliably.>

## Steps to reproduce

1. <Starting page and required prior state.>
2. <The action that causes the problem.>
3. <The error observed.>

## Shared attachments

<Add approved images or bundles that the Agent can actually read. Do not use absolute paths that only the server can read.>

## Agent instructions

Read the image and the feedback first, state the problem as you understand it, and acknowledge under your own identity (reading and queueing are not an acknowledgment). When the browser must be operated, acquire a lease with `lensdock lease acquire <workspace-id> --mode agent --feedback-id <feedback-id> --operator-token-file <agent-token-file> --output <lease-file>`, confirm the current target and observe it again. Do not click using coordinates from an old screenshot, and a `drawing` image has no clickable page coordinates at all. Do not read unauthorized original screenshots, cookies or the Profile. When finished, report a summary of the changes, the verification steps and a new verification screenshot, then wait for human confirmation.
