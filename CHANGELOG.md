# Changelog

All notable changes to `ss7-3eai-gateway-client` are documented in this file.
Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versioning follows [SemVer](https://semver.org/) (pre-1.0: any 0.x change may be breaking).

## [Unreleased]

## [0.3.0] - 2026-10-14

### Added

- Call routing: `SendRoutingInfo` (MAP op 22, a basic call interrogation towards the HLR, the
  gateway as GMSC) on `map.routing.request` → `map.routing.response`. Needs gateway 0.7.6 or later;
  an older gateway leaves the request unanswered until the deadline. The subjects are outside
  `map.sri.*`, so NATS permissions can grant SRI-for-SM without call routing.
  - `RoutingRequest` takes the MSISDN and, optionally, the HLR's address. The GMSC address is the
    gateway's own; the gateway ignores every other request field.
  - The answer carries `routing_info` with the roaming number, forwarding data and CAMEL routing
    information. The roaming number and forwarded-to number are `AddressString` objects.
    Forwarding options are decoded.
  - `routing_info` also carries the VMSC address, basic MSISDN, number portability status, basic
    service and active SS codes.
  - The IMSI is in `imsi`, and any location or state is in `subscriber_info`.
- MAP error codes SendRoutingInfo adds:

  | Constant | Code |
  |---|---|
  | `MAPErrBearerServiceNotProvisioned` | 10 |
  | `MAPErrForwardingViolation` | 14 |
  | `MAPErrCUGReject` | 15 |
  | `MAPErrAbsentSubscriber` | 27 |
  | `MAPErrNumberChanged` | 44 |
  | `MAPErrBusySubscriber` | 45 |
  | `MAPErrNoSubscriberReply` | 46 |
  | `MAPErrORNotAllowed` | 48 |

### Changed

- `New` no longer subscribes to every response subject up front. A request subscribes to its own
  operation's response subject the first time that operation is used. An account limited to some
  operations therefore no longer gets permission violations for the others.
- AnyTimeInterrogation's MSISDN and ProvideSubscriberInfo's VLR number are limited to 16 digits,
  the most an ISDN-AddressString holds. SendRoutingInfo's MSISDN has the same limit.

## [0.2.0] - 2026-10-14

### Added

- The MAP location interface: `AnyTimeInterrogation` (op 71, towards the HLR) on
  `map.ati.request` → `map.ati.response` and `ProvideSubscriberInfo` (op 70, towards the VLR) on
  `map.psi.request` → `map.psi.response`. `Message` carries `requested_info` in requests and
  `subscriber_info` in answers: location (cell, service area or location area, VLR/MSC/SGSN
  numbers, age), subscriber state, EPS location, IMEI, MS Classmark 2 and number portability.
  Parameter key `vlr_number`; `MAPErrATINotAllowed` (49). The gsmSCF-Address an HLR screens
  AnyTimeInterrogation on is the gateway's configuration, not something a request can set.
  The location requests check that identities and addresses are decimal digits before publishing.
- `StatusRejected` / `ErrCodeRejected` (-4): the gateway answers a request it refuses before
  starting a dialogue instead of staying silent (gateway 0.7.4 and later).

### Fixed

- `E164()` returned nature of address 4, which in a TS 29.002 AddressString is "subscriber
  number"; an international number is 1. It now returns 1, and the `AddressString` documentation
  says which coding `Nature` uses. With 0.1.0, every address built by `E164()` — the service centre
  address, the network node number — reached the network as a subscriber number. Upgrade, and
  replace any hand-built `Nature: 4` meant as international with 1.

## [0.1.0] - 2026-10-14

### Added

- The NATS wire contract of ss7-3eai-gateway's MAP SMS interface: subjects, the `Message`
  schema, parameter keys, status values and gateway failure codes.
- `Client`: `SendRoutingInfoForSM` and `MTForwardSM` with correlation-ID matching, request
  validation and a default deadline; `SubscribeMO` for MO-Forward-SM, with optional queue groups.
- `StatusRemoteError` is emitted by the gateway: a MAP ReturnError arrives with the MAP error code
  in `Code`; `MAPErr*` constants name the codes SRI-for-SM and MT-ForwardSM can return.
