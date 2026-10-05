# Pilot with five developers

The goal is to check whether another developer can obtain a useful result independently and continue
after an engine restart. This is a research plan; participants and results are not yet available.

## Preparation

Hand over to the participant [ready CLI](https://github.com/michael-bill/knotra/releases/latest),
[first launch](first-run.md), and three of their own small source materials without secrets. Start
with bundled fictional sources, then replace them with real data while preserving labels used by the
deterministic checker code. Clarify OS, Docker/Ollama availability, and model. For a cloud model,
the participant enters their key into the engine environment.

Conduct each session separately. Observe without hints for the first ten minutes. Record actions and
errors with the participant's permission; do not include keys or file contents in the shared note.

## Task

Obtain `greeting.txt`; open **Research, verify and approve**; explain how scores were calculated;
wait for human review; stop and restart the engine; approve and find the final ZIP and approval
record, or cancel the run if publication should be refused. Then repeat with a small task of your
own.

## Session Card

| Participant | OS / model | Minutes to first file | Minutes to dossier | Hints / Stop location | Recovery | Usefulness 1–5 | Would you repeat? |
| ----------- | ---------- | --------------------- | ------------------ | --------------------- | -------- | -------------- | ----------------- |
| P1          |            |                       |                    |                       |          |                |                   |
| P2          |            |                       |                    |                       |          |                |                   |
| P3          |            |                       |                    |                       |          |                |                   |
| P4          |            |                       |                    |                       |          |                |                   |
| P5          |            |                       |                    |                       |          |                |                   |

For each failure, save the exact command, diagnostic code, interface step, and what the human
expected. Account separately for model loading time and generation time. An automatic pass does not
count as a user session.

## Post-Task Questions

- What did you expect to see on first open? What had to be searched for?
- Why was this specific option chosen? Which facts are verified by code, and which require your
  verification?
- Was it clear what would happen after approval and after restart?
- What work task of yours could this solve? What do you use today for it?
- Would you launch a second process without the project author? If not, what specifically prevents
  it?

## Findings and next steps

First benchmark: at least 4 out of 5 obtain the file without help, at least 3 pass recovery and name
their own task for reuse. This is a pilot hypothesis, not a confirmed product metric. First fix
recurring obstacles; then conduct two more independent sessions. Add features after identified
limitations, not by number of requests.

Fill in the summary: recurring difficulties, three changes by priority, what not to do now, which
facts changed the original hypothesis. Invitations and real sessions are organized by the project
owner.
