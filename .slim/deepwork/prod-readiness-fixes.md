# Sonarr Remediator - Prod Readiness Fixes

## Goal
Fix all identified bugs and gaps to make the codebase ready for prod testing.

## Issues to Fix

### Phase 1 - Core wiring (parallel, each in separate file)
1. Wire up detectors in queue monitor
2. Fix retry scheduler (retryable error filtering + path translation)
3. Fix dashboard (health integration, ignore duration, fake sonarr health)
4. Fix safety engine (decision ID uniqueness, cooldown logic)
5. Fix quality defs nil handling + confidece typo

### Phase 2 - Queue monitor improvements
6. Queue monitor dedup
7. Cleanup engine queue item cross-reference check

### Phase 3 - Validation
8. Build test
9. Review all diffs

## Current Status
- Phase 1: starting
- Phase 2: pending
- Phase 3: pending
