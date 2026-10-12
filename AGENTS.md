# Repository Agent Guidance

## Deployment compatibility

Treat any change that requires an operator to modify an existing Docker or Podman
deployment, beyond updating the image version, as a breaking change. This includes
required changes to configuration, environment variables, commands, healthchecks,
mounts, permissions, container identity, networking, or deployment lifecycle.

Identify these changes as breaking in release planning and change records, and
document the required migration. An image-version-only update is exempt from
this rule; it does not exempt other breaking behavior in that release.
