# Browser Manual Import and Matching Evidence

The operator confirmed that both examples were matched automatically when
opening Manual Import. No show, season, or episode correction was necessary.

Evidence combines live GET responses, the operator's Firefox HAR for the
non-upgrade example, and Sonarr's frontend/backend source at tag
`v4.0.20.3014`, commit `cab419ade8ac7fcab5bf80394ee492abd35d5f5a`.
No live import, deletion, or reprocessing POST was submitted during capture.

## What the Browser Does

1. Opening the dialog loads files from:

   ```http
   GET /api/v3/manualimport?downloadId=download-008&filterExistingFiles=false
   ```

   The identifier in this example is an anonymized alias. The frontend defaults
   to showing all files (`filterExistingFiles=false`). It supplies the tracked
   download ID, which the server uses to resolve the download's output path.

2. Sonarr returns one resource per previewed file: its path, matched series,
   matched episodes, quality/revision, custom formats and score, and
   `rejections` with human-readable `reason` and `type` fields.

3. The browser displays those matches and a rejection warning when
   `rejections` is nonempty. There is no `isUpgrade` field in this response.
   The observed row display does not calculate a strict upgrade verdict.

4. Changing a show, season, episode, or other import field causes a separate
   `POST /api/v3/manualimport` to reprocess proposed values and return new
   verdicts. This endpoint evaluates the selection; it is not the import
   command. Its source was inspected, but it was not exercised on the live
   instance. The confirmed examples require no edits or reprocessing POST.

5. Clicking Import Selected constructs a `ManualImport` command, using each
   selected preview file's path, folder name, series ID, episode IDs, quality,
   languages, release group, indexer flags, release type, and tracked download
   ID. The tracked-download flow uses import mode `auto`.

   ```http
   POST /api/v3/command
   ```

   This command changes Sonarr and media state. It was not executed. The
   frontend also has a separate episode-file metadata-update path for edits
   to existing library files; that path is outside the rewrite's scope.

## Observed Examples

The supplied release hashes were found in queue titles/output paths, not in
the queue's `downloadId` field. They were used only to locate the corresponding
queue record. Subsequent API calls used the actual Sonarr `downloadId`.
Raw hashes are excluded from the stored evidence.

| Operator example | Snapshot alias | Automatic match and current verdict |
| --- | --- | --- |
| First example, described as an upgrade | `download-009` | Matches series `1010`, episode `2017`; preview score 807 and no rejections. Current existing file also has score 807, the same quality/revision, and the same size. |
| Second example, described as a non-upgrade | `download-008` | Matches series `1009`, episode `2016`; preview score 304 versus existing 805. Sonarr reports "Not a Custom Format upgrade for existing episode file(s)" with type `permanent`. |

The first observation establishes automatic matching and preview acceptance.
It does not establish a strict upgrade over the current existing file, or
whether that file was imported earlier. Actual strict score improvements are
present in `download-005` (406 to 807) and `download-011` (-697 to 804).

[browser-observation.json](browser-observation.json) preserves the HAR's GET
method, query parameters, HTTP 200, application version, and selected response
fields, using the same aliases as [snapshot.json](snapshot.json). It is a
normalized observation, not the original HAR. Request/response headers,
cookies, private hostname, raw identities, extensive series metadata, and the
unrelated Sentry request were excluded. The telemetry endpoint is not part of
the service's interface.

## Automation Contract

The service can obtain the browser's automatic match and rejection verdict
through the same GET endpoint. It needs no browser automation or independent
title parser for these examples.

Every proposed import still needs a fresh preview and the agreed safety policy.
The backend's manual-import command constructs an approved import decision
from the supplied file selections; it is not a substitute for the preview
checks. Never submit a rejected or ambiguously matched file simply because
the browser exposes an Import Selected button.

The approved product policy requires strict improvement before replacement.
An equal-quality/equal-score accepted preview can qualify for separately
enabled redundancy cleanup after all cleanup checks pass.

## Source References

All references are pinned to the version running during evidence capture.

- [Dialog fetch and import submission](https://github.com/Sonarr/Sonarr/blob/cab419ade8ac7fcab5bf80394ee492abd35d5f5a/frontend/src/InteractiveImport/Interactive/InteractiveImportModalContent.tsx)
- [Row rejection display and edit callbacks](https://github.com/Sonarr/Sonarr/blob/cab419ade8ac7fcab5bf80394ee492abd35d5f5a/frontend/src/InteractiveImport/Interactive/InteractiveImportRow.tsx)
- [GET and reprocessing POST requests](https://github.com/Sonarr/Sonarr/blob/cab419ade8ac7fcab5bf80394ee492abd35d5f5a/frontend/src/Store/Actions/interactiveImportActions.js)
- [Manual import API controller](https://github.com/Sonarr/Sonarr/blob/cab419ade8ac7fcab5bf80394ee492abd35d5f5a/src/Sonarr.Api.V3/ManualImport/ManualImportController.cs)
- [Manual import resource and rejection fields](https://github.com/Sonarr/Sonarr/blob/cab419ade8ac7fcab5bf80394ee492abd35d5f5a/src/Sonarr.Api.V3/ManualImport/ManualImportResource.cs)
- [Matching, preview, and command execution](https://github.com/Sonarr/Sonarr/blob/cab419ade8ac7fcab5bf80394ee492abd35d5f5a/src/NzbDrone.Core/MediaFiles/EpisodeImport/Manual/ManualImportService.cs)
- [Sonarr's import upgrade checks](https://github.com/Sonarr/Sonarr/blob/cab419ade8ac7fcab5bf80394ee492abd35d5f5a/src/NzbDrone.Core/MediaFiles/EpisodeImport/Specifications/UpgradeSpecification.cs)
