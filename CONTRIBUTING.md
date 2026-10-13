# Contributing

Thanks for considering a contribution.

## How this repository is published

Development happens in a private repository; this one is published as a full-tree snapshot per
release. Open issues and pull requests here as normal — they are read and answered here. An
accepted change is applied upstream and appears in the next published snapshot, credited in
`CHANGELOG.md`. If attribution matters to you, say so in the pull request.

## This module is the wire contract

The subjects, the `Message` JSON schema, the parameter keys, and the status and failure codes in
this module are what [ss7-3eai-gateway](https://github.com/3eAI-Labs/ss7-3eai-gateway) speaks, and
what non-Go consumers implement against. Renaming a JSON field or changing a token is a breaking
change for every one of them, which `TestMessageWireFieldNames` exists to catch. Additions are
fine; removals and renames need a major version.

Only interface the gateway implements end to end belongs here. If you need a new operation, the
gateway has to support it first.

## Building and testing

```bash
go vet ./...
go test -race ./...
```

The tests run an embedded NATS server and need nothing else.

## Pull requests

- Keep the change focused, and add or update tests for behaviour changes
- Write comments in English, and only where they explain something the code cannot say itself
- New Go files start with the licence header used by the existing ones
- Update `CHANGELOG.md` under `## [Unreleased]` for anything user-visible
- Accept the [contributor licence agreement](CLA.md) when the bot asks — once, on your first
  pull request

## Licence and contributor agreement

This module is released under the [Apache License 2.0](LICENSE), and every contribution is
published under it. Before a contribution can be merged you also have to accept the
[3eAI Labs Contributor Licence Agreement](CLA.md) — one pull request comment, once, covering
ss7-3eai-gateway as well:

```
I have read the CLA Document and I hereby sign the CLA
```

If you are contributing in the course of your employment, say so in the pull request — see section
4.2 of the agreement.
