# Changelog

All notable changes to `ss7-3eai-gateway-client` are documented in this file.
Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versioning follows [SemVer](https://semver.org/) (pre-1.0: any 0.x change may be breaking).

## [Unreleased]

## [0.1.0] - 2026-10-14

### Added

- The NATS wire contract of ss7-3eai-gateway's MAP SMS interface: subjects, the `Message`
  schema, parameter keys, status values and gateway failure codes.
- `Client`: `SendRoutingInfoForSM` and `MTForwardSM` with correlation-ID matching, request
  validation and a default deadline; `SubscribeMO` for MO-Forward-SM, with optional queue groups.
- `StatusRemoteError` is emitted by the gateway: a MAP ReturnError arrives with the MAP error code
  in `Code`; `MAPErr*` constants name the codes SRI-for-SM and MT-ForwardSM can return.
