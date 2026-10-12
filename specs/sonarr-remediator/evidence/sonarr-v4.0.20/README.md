# Anonymized Sonarr API Evidence

Captured on 2026-10-12 from Sonarr `4.0.20.3014`, with explicit permission for
read-only SSH and API access. Every API request used GET. No configuration,
queue, library, download-client, or service changes were requested.

[snapshot.json](snapshot.json) contains selected API fields with their original
JSON types. It is evidence for API contracts and future fixtures, rather than
an unmodified export or an atomic snapshot of the whole installation.

## Coverage

- A complete queue read, including unknown-series entries: 14 records.
- Import previews for all 14 completed downloads: 17 file verdicts.
- Current episode resources for queue identities and preview matches.
- Current episode-file resources for episodes that have files.
- The two relevant quality profiles, including grouped qualities and scores.
- The media-management setting controlling proper/repack preference.
- Sonarr version and capture metadata.
- [Browser-flow evidence](BROWSER_FLOW.md) and a normalized observation from
  the operator's HAR for the non-upgrade example.

The queue contains 11 `importBlocked` and three `importPending` entries. Seven
records lack series/episode identity; ten lack an added timestamp. All records
have queue status `completed`, so queue status alone cannot establish that an
import is blocked and eligible for intervention.

## Case Matrix

These are policy examples grounded in the captured fields. They do not
authorize live actions; waits, exclusions, competing work, and fresh checks
still apply.

| Download alias | Evidence | Expected treatment |
| --- | --- | --- |
| `download-001` | Unknown queue identity; clean preview resolves episode `2008`, which has no file | Candidate for missing-episode recovery |
| `download-005` | Clean preview; grouped-equivalent quality; custom-format score 807 versus existing 406 | Candidate for upgrade, subject to the current profile |
| `download-011` | Known episode; clean preview; grouped-equivalent quality; score 804 versus existing -697 | Candidate for upgrade, subject to the current profile |
| `download-003`, `download-009`, `download-012`, `download-014` | Clean previews, but current quality/revision and score equal the existing file | Do not import; possible redundancy cleanup after every cleanup gate passes |
| `download-008` | Preview rejects custom-format downgrade; candidate 304 versus existing 805 | Do not import; possible redundancy cleanup after every cleanup gate passes |
| `download-004`, `download-007` | Remaining preview files are older episodes with score 0 versus existing 807; the queue's expected episode is missing and absent from the preview | Preserve entire downloads; a non-upgrade message alone cannot prove a pack is redundant |
| `download-010` | TheXEM mapping is unconfirmed; expected episode has no file | Preserve and report mapping intervention |
| `download-002`, `download-006`, `download-013` | Preview says "Unable to determine if file is a sample"; state is `importPending` | Preserve; do not force import or classify as unusable |

`download-008` and `download-014` resolve to the same episode (`2016`). Download
identity, episode identity, and shared-download boundaries must remain distinct.

## Anonymization

Credentials never entered the exported data. Raw responses were processed in
memory on the remote machine; only anonymized projections were returned.

Series, episodes, queue records, download identifiers, file identifiers,
profiles, custom formats, and quality groups use surrogate identities. Release
titles, series/episode titles, client/indexer names, release groups, filenames,
and filesystem paths are replaced with synthetic labels. Mappings are
consistent across resource references; no reversible mapping is retained.

Standard Sonarr quality identifiers and names, protocol/status enums, rejection
wording, numeric scores, sizes, and policy values are retained because they
explain the API behavior. Named custom formats in rejection text are replaced
consistently with the profile and preview labels.

API timestamps are shifted together to a synthetic capture anchor of
`2026-01-01T00:00:00Z`; relative differences, nulls, and missing fields are
preserved. The real capture time appears only in provenance metadata.

## Limits

- Endpoint reads were sequential; state could change between responses.
- Omitted fields are a deliberate projection and do not establish that those
  fields were absent from the original response.
- No history or command lifecycle was captured. The snapshot cannot establish
  prior import success, consecutive failures, or how long an item was blocked.
- One successful snapshot does not demonstrate restart, pagination races,
  missing-client errors, oversized responses, or mutation outcomes. Those need
  separate controlled fixtures and integration validation.
- Preview rejections expose `reason` text and `type`, not a stable machine
  reason code. `permanent` includes unresolved mapping and sample checks, so it
  cannot itself authorize deletion.
