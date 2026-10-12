# Recovered Specification Sources

The rewrite starts from an empty branch. Historical documents were recovered
without restoring implementation files or applying the saved worktree.

## Source Revisions

- Committed project: `main` at
  `71566c26107e8410d12489e55481f061e6bc3479`.
- Saved uncommitted documents: stash commit
  `f31f1f923839c3a98c4627273b307964386f38a1`, created with the description
  `pre-clean-rewrite worktree`.
- Readable copies of both revisions:
  `/tmp/sonarr-remediator-original-specs/main/` and
  `/tmp/sonarr-remediator-original-specs/saved-worktree/`.
  These copies are temporary; Git retains the source revisions.

Recover an individual document without changing the worktree with:

```sh
git show 71566c26107e8410d12489e55481f061e6bc3479:specs/sonarr-remediator/PRODUCT.md
git show f31f1f923839c3a98c4627273b307964386f38a1:specs/sonarr-remediator/PRODUCT.md
```

## Documents Recovered

- `SPEC.md` and `DOCKER_SPEC.md`.
- `OPERATIONAL_STATE_PLAN.md`.
- `specs/sonarr-remediator/PRODUCT.md` and `TECH.md`.
- `specs/active-deployment/PRODUCT.md`, `TECH.md`, `BASELINE.md`,
  `ASSESSMENT.md`, and `HARDENING.md`.

## Findings Relevant to Discovery

- The original service already used Go and communicated only with Sonarr.
- Sonarr's API can preview and import tracked downloads without mounting the
  media directory in the remediator container.
- Scope expanded into import confidence scoring, duplicate reconciliation,
  quality comparisons, delayed retries, and durable work queues. Each feature
  needs a deliberate decision before inclusion in the rewrite.
- Historical policy paths applied different checks. The rewrite needs one
  agreed import policy across every recovery path.
- The historical deployment assessment reports 20 of 21 queue records with
  missing or null added timestamps. A missing timestamp cannot safely establish
  that an item has waited long enough.
- A warning queue status can coexist with a still-downloading tracked state.
  Warning text alone cannot establish completion.
- Unknown-series queue entries sometimes produce unambiguous episode matches
  in Sonarr's import preview.
- Some previews rejected files for custom-format policy or unresolved TheXEM
  mappings; others matched unexpected episodes. These are distinct cases that
  need explicit behavior.
- A download can contain several episodes or files. Removing one queue row
  can affect the shared download.
- API acknowledgement and disappearance from the queue do not alone establish
  that the intended episode file was successfully imported.
- The recovered assessment is historical evidence, not a fresh observation of
  any running deployment.

## New Read-Only Evidence

[API evidence](evidence/sonarr-v4.0.20/README.md) records the live read-only
capture from 2026-10-12. [Browser-flow evidence](evidence/sonarr-v4.0.20/BROWSER_FLOW.md)
combines the live previews, the operator's supplied HAR, and source pinned to
Sonarr `v4.0.20.3014` at `cab419ade8ac7fcab5bf80394ee492abd35d5f5a`.

The capture includes genuine upgrade candidates, equal accepted previews,
rejected non-upgrades, unresolved TheXEM mapping, sample-detection failures,
missing timestamps, and multi-file downloads missing their intended episode.
Raw credentials, session cookies, release hashes, private hostnames, and media
identities were excluded from saved evidence.
