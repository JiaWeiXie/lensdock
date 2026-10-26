# Issue / task handoff

**Feedback ID:** `<feedback-id>`  **Revision:** `<revision>`

**Workspace:** `<workspace-id>`  **Image source:** `<desktop | page-viewport | drawing>` (`drawing` is an explanatory image on a blank canvas and has no browser page)

Fill in only the sections you need and delete the rest. The product does not require any prose: purely visual feedback carries an image and annotations with an empty description, and the Gateway supplies the title `Visual feedback`. Never invent expected or actual results the human did not give.

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

Read the image and the feedback first, state the problem as you understand it, and acknowledge under your own identity. Reading and queueing are not an acknowledgment, and "Sent" in the UI means queued or read, not started. When the browser must be operated, take a fresh full-desktop screenshot with `lensdock desktop screenshot --project <project-dir> --feedback-id <feedback-id> --output <png>` (or `--workspace <workspace-id> --token-file <agent-token-file>` instead of `--project`). The command acquires and releases its own AGENT lease. Look at that screenshot and choose pointer coordinates from it; the DOM is an optional aid only. Do not click using annotation coordinates or an old screenshot, and a `drawing` image has no clickable page coordinates at all. Do not read unauthorized original screenshots, cookies or the Profile. When finished, resolve with a summary of the changes, the verification steps and a new verification screenshot, then wait for human confirmation: Done in the UI is the Agent's own report until a human verifies it.
