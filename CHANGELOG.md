# Changelog

## [0.4.0](https://github.com/calmcacil/sonarr-remediator/compare/v0.3.6...v0.4.0) (2026-08-16)


### Features

* preview-based recovery on v4; removeTorrentErrors rule; container healthcheck ([5845e40](https://github.com/calmcacil/sonarr-remediator/commit/5845e40f220f0b5349509722cc48e050ebf7bbf9))
* resolve unknown-series downloads via the manual-import path ([02049c7](https://github.com/calmcacil/sonarr-remediator/commit/02049c738e74aa3e13c2601b1b77aa88995a25f3))


### Bug Fixes

* fetch unknown-series queue items; per-download cooldown for episode-less items ([19566c6](https://github.com/calmcacil/sonarr-remediator/commit/19566c6477f0770c24f95bc6c2c67485e3feeb67))
* honor Sonarr's import rejections in unknown-series resolution ([b09f340](https://github.com/calmcacil/sonarr-remediator/commit/b09f3406ac390c40ed09f2b8e8c698d9805e6c52))
* stop per-poll re-detection and action.skipped spam for stuck items ([bf93e59](https://github.com/calmcacil/sonarr-remediator/commit/bf93e5945a1bb336d405fa164ca0201511fbb71b))
