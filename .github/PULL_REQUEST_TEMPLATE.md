<!--
Development happens in a private repository and this one is published as a snapshot per release,
so your commit will not appear verbatim in this repository's history. The change itself is applied
upstream and credited in CHANGELOG.md. See CONTRIBUTING.md.
-->

## What this changes

<!-- And why. Link the issue it addresses. -->

## Checks

- [ ] `go vet ./...` and `go test -race ./...` pass
- [ ] No JSON field, subject or status token renamed or removed (that is a major version)
- [ ] Tests added or updated for behaviour changes, and they fail without the change
- [ ] `CHANGELOG.md` updated under `## [Unreleased]` if this is user-visible
