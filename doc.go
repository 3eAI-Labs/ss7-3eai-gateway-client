// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: Apache-2.0

// Package ss7client is the NATS client for ss7-3eai-gateway: the subjects, the
// message schema and a typed request/response client for the MAP interfaces
// the gateway implements.
//
// This module is licensed under the Apache License 2.0, separately from the
// gateway server (AGPL-3.0-only), so an SMSC or any other application can link
// it without taking on the server's licence. The dependency runs one way: the
// server imports this module for the wire contract, this module never imports
// the server.
//
// # Interface covered
//
// Only the interface the gateway implements end to end is exposed:
//
//   - SRI-for-SM (MAP op 45): [Client.SendRoutingInfoForSM], subjects
//     [SubjectSRIRequest] → [SubjectSRIResponse].
//   - MT-Forward-SM (MAP op 44): [Client.MTForwardSM], subjects
//     [SubjectMTSMSRequest] → [SubjectMTSMSResponse].
//   - MO-Forward-SM (MAP op 46), gateway → application: [Client.SubscribeMO],
//     subject [SubjectMOSMS].
//   - AnyTimeInterrogation (MAP op 71) towards the HLR:
//     [Client.AnyTimeInterrogation], subjects [SubjectATIRequest] →
//     [SubjectATIResponse].
//   - ProvideSubscriberInfo (MAP op 70) towards the VLR:
//     [Client.ProvideSubscriberInfo], subjects [SubjectPSIRequest] →
//     [SubjectPSIResponse].
//   - SendRoutingInfo (MAP op 22) towards the HLR, the gateway as GMSC
//     (gateway 0.7.6 and later): [Client.SendRoutingInfo], subjects
//     [SubjectRoutingRequest] → [SubjectRoutingResponse].
//
// # Wire model
//
// Every message is a JSON-encoded [Message]. Requests are published on a
// request subject; the gateway queue-subscribes, runs the MAP dialogue and
// publishes the outcome — result or failure — on the matching response
// subject with the request's correlation_id. Response subjects are broadcast:
// every client instance receives every response and keeps only its own, by
// correlation_id. A [Client] subscribes to a response subject on the first
// request that needs it.
//
// A request the gateway refuses before starting a dialogue — a missing or
// invalid field, no capacity for a new dialogue — is answered with
// [StatusRejected] from gateway 0.7.4 on; earlier gateways send nothing, and
// nothing can be sent for a request that is not valid JSON. [Client]
// therefore validates the fields the gateway requires before publishing, and
// applies a default deadline when the context has none.
package ss7client
