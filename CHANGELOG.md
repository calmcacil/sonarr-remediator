# Changelog

## [0.5.1](https://github.com/calmcacil/sonarr-remediator/compare/v0.5.0...v0.5.1) (2026-08-16)


### Bug Fixes

* route qBittorrent warning items through torrent error removal ([#11](https://github.com/calmcacil/sonarr-remediator/issues/11)) ([1870642](https://github.com/calmcacil/sonarr-remediator/commit/187064289f47e65fa8a8413d2ae41d2314016d8c))

## [0.5.0](https://github.com/calmcacil/sonarr-remediator/compare/v0.4.0...v0.5.0) (2026-08-16)


### Features

* switch logging to key=value text with a type field ([#8](https://github.com/calmcacil/sonarr-remediator/issues/8)) ([8adfc7c](https://github.com/calmcacil/sonarr-remediator/commit/8adfc7cb79a08f3f6cff2d1c628598396c315011))


### Bug Fixes

* distinguish Sonarr auth failures with a dedicated event ([#8](https://github.com/calmcacil/sonarr-remediator/issues/8)) ([8adfc7c](https://github.com/calmcacil/sonarr-remediator/commit/8adfc7cb79a08f3f6cff2d1c628598396c315011))

## [0.4.0](https://github.com/calmcacil/sonarr-remediator/compare/v0.3.6...v0.4.0) (2026-08-16)


### Features

* preview-based recovery on v4; removeTorrentErrors rule; container healthcheck ([5845e40](https://github.com/calmcacil/sonarr-remediator/commit/5845e40f220f0b5349509722cc48e050ebf7bbf9))
* resolve unknown-series downloads via the manual-import path ([02049c7](https://github.com/calmcacil/sonarr-remediator/commit/02049c738e74aa3e13c2601b1b77aa88995a25f3))


### Bug Fixes

* fetch unknown-series queue items; per-download cooldown for episode-less items ([19566c6](https://github.com/calmcacil/sonarr-remediator/commit/19566c6477f0770c24f95bc6c2c67485e3feeb67))
* honor Sonarr's import rejections in unknown-series resolution ([b09f340](https://github.com/calmcacil/sonarr-remediator/commit/b09f3406ac390c40ed09f2b8e8c698d9805e6c52))
* stop per-poll re-detection and action.skipped spam for stuck items ([bf93e59](https://github.com/calmcacil/sonarr-remediator/commit/bf93e5945a1bb336d405fa164ca0201511fbb71b))
