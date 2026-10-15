# Changelog

All notable changes to `ss7-3eai-gateway-client` are documented in this file.
Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versioning follows [SemVer](https://semver.org/) (pre-1.0: any 0.x change may be breaking).

## [Unreleased]

## [0.6.1] - 2026-10-15

### Added

- **`RejectCause`** on `AuthenticationAnswer`, `PurgeAnswer` and `LocationUpdateAnswer`, and
  `ErrorCause` on the wire: what qualifies the error a request is refused with (TS 29.002 §7.6.1).
  - unknownSubscriber's diagnostic, which it may go without: `CauseIMSIUnknown`, or
    `CauseGPRSEPSSubscriptionUnknown` — TS 29.002 names the latter for a sendAuthenticationInfo
    from an SGSN for a subscriber without GPRS.
  - roamingNotAllowed's cause: `CausePLMNRoamingNotAllowed` (the default, also when none is
    given), `CauseOperatorDeterminedBarring` or `CauseSupportedRATTypesNotAllowed`.
  - A cause that does not qualify the answer's error is refused as a bad answer, as other bad
    answers are: systemFailure for authentication and location updates, an abort for purging.
  - A gateway before 0.7.10 ignores the field: the error goes without its cause. Needs gateway
    0.7.10 or later for the cause to reach the network.

## [0.6.0] - 2026-10-15

### Added

- Authentication and MS purging with the gateway as HLR (TS 29.002 §8.5.2, §8.1.6); needs gateway
  0.7.9 or later.
  - **`ServeAuthentication`** answers a VLR's or SGSN's sendAuthenticationInfo on `map.sai`, as
    NATS request and reply: the application is the AuC. The request carries the number of
    vectors wanted, the requesting node type and PLMN, re-synchronisation data (RAND, AUTS), and
    the EPS and UE usage type indications (`RequestingNodeType` empty when the request has
    none); the answer is triplets, quintuplets or EPS vectors
    (hex), the UE usage type, or `Reject` with one of the operation's errors. No vectors is the
    empty result. `Authentication.VectorLimits` gives how many of each kind the request allows.
    An answer that breaks the rules (both triplets and quintuplets, more vectors of a kind than
    allowed, a wrong length) is sent as systemFailure and reported to the decode error
    handler.
  - **`ServePurges`** answers purgeMS on `map.purge`: freeze TMSI / P-TMSI / M-TMSI, or `Reject`
    with dataMissing, unexpectedDataValue or unknownSubscriber.
  - `Message` fields `authentication` and `purge`; message types `MAP_SAI`, `MAP_PURGE`;
    `Node*` requesting node type names.

## [0.5.0] - 2026-10-15

### Added

- Location management with the gateway as HLR (TS 29.002 §8.1, §8.8.1, §10.2); needs gateway
  0.7.8 or later. The application is the HLR's location management (TS 23.012), the gateway
  speaks MAP to the VLRs.
  - **Location updates:** `ServeLocationUpdates` answers a VLR's updateLocation on `map.ul`,
    turn by turn, as NATS request and reply — the MAP process in the HLR asks the application at
    each decision (TS 29.002 figure 19.1.1/13).
    - The turn `update_location` carries what the VLR sent: MSC and VLR numbers, LMSI, VLR
      capability, IMEISV, the calling GT.
    - The answer inserts subscriber data, accepts (updateLocation's result, with the gateway's
      HLR number) or rejects with one of updateLocation's errors.
    - After an insert, the turn `insert_result` says what the VLR did not take, or why the data
      failed; the application answers again.
    - A location update the gateway does not end with the application's accept or reject gets
      exactly one `end` turn, which is not answered: the VLR or the network ended it, the gateway
      refused it on its own account (no answer by `answer_by`, an answer it cannot carry out),
      or the answer could not be sent. The end comes after the turn in flight; an end after an
      answer means the answer was not carried out.
    - Each turn runs on its own goroutine with a context ending at the turn's `answer_by`.
  - **`SubscriberData`:** the circuit-switched profile — MSISDN, category, status, bearer and
    teleservices, call forwarding, call barring, other SS data, ODB, regional subscription,
    network access mode, access restriction. Codes are hex and names the ASN.1 value names.
    It is checked before it is sent. CUG, eMLPP, VBS/VGCS and CAMEL subscription data are not
    carried.
  - **Requests to a VLR** (named by `ParamVLRNumber`, as for ProvideSubscriberInfo):
    `CancelLocation` (`map.cl.*`), `ProvideRoamingNumber` (`map.prn.*`, the roaming number and
    VMSC address back) and `InsertSubscriberData` (`map.isd.*`, stand-alone).
  - `Message` fields `location_update`, `subscriber_data`, `insert_result`, `cancel_location`,
    `roaming_number_enquiry`.
  - MAP error codes `MAPErrRoamingNotAllowed` (8), `MAPErrNoRoamingNumberAvailable` (39).
  - The name tables in bit or value order (`ODBGeneralNames`, `RATTypeNames`, …).

## [0.4.0] - 2026-10-15

### Added

- USSD sessions (TS 29.002 §11.9), with the gateway as gsmSCF; needs gateway 0.7.7 or later.
  - **Subscriber-initiated:** a `begin` event on `map.ussd.event.<route>` (`SubscribeUSSD`). The
    route is the one the dialled string matches in the gateway's `map.ussd_routes`, else
    `default`. The application then:
    - prompts with `USSDRequest`, which returns the reply; an empty reply is allowed;
    - shows text with `USSDNotify`;
    - ends with `USSDRelease` and the final text, or `USSDReject` and a MAP error that
      processUnstructuredSS-Request allows.
  - **Network-initiated:** `StartUSSD` on `map.ussd.start`. The same commands continue it, and
    `USSDRelease` without text ends it.
  - **Endings:** every session ends exactly once, and the gateway publishes one `end` event for it
    on its route, whoever ended it. Status and code say how; a failed command reports the same
    ending as an error. Core NATS delivers at most once, so keep a timeout of your own per
    session; a reply the gateway cannot publish ends its session (`ABORTED`).
  - **Commands** go to every node on `map.ussd.command`; the node holding the session acts. That
    node answers `REJECTED` to a command it cannot carry out: an unknown or ended session, a
    request or notify while another is outstanding, or the wrong release or reject for the
    session's kind. A release or reject is carried out with a request or notify outstanding,
    which then fails with `ABORTED`.
    Session IDs are opaque and unguessable, at most 64 characters.
  - **Text** is encoded by the gateway: GSM 7-bit when it fits the default alphabet, else UCS2.
    Alternatively the application gives the DCS and data (base64 in JSON). Answers carry DCS,
    data and the decoded text.
  - **Wire format:** Message field `ussd`; the reject code travels in `ussd.error`, so `error_code`
    stays response-only.
  - **Timeouts:** USSD calls without a deadline get `DefaultUSSDTimeout` (3 min 10 s;
    `WithUSSDTimeout`), not the 40 s of the other operations. After a deadline the session may still
    be open. The gateway's `map.ussd_session_timeout` (1–10 min, default 180 s) bounds each wait.
  - **Begins** are delivered even when the string cannot be read: `Message` nil, or `Message.Text`
    empty with `Detail` saying why.
  - **New MAP error codes:** `MAPErrUnknownAlphabet` (71), `MAPErrUSSDBusy` (72).

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
