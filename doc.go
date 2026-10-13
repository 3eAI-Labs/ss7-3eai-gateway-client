// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: Apache-2.0

// Package ss7client is the NATS client for ss7-3eai-gateway: the subjects, the
// message schema and a typed request/response client for the MAP SMS path.
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
//
// # Wire model
//
// Every message is a JSON-encoded [Message]. Requests are published on a
// request subject; the gateway queue-subscribes, runs the MAP dialogue and
// publishes the outcome — result or failure — on the matching response
// subject with the request's correlation_id. Response subjects are broadcast:
// every client instance receives every response and keeps only its own, by
// correlation_id.
//
// A request the gateway rejects before starting a dialogue (missing field,
// undecodable JSON) produces no response today; the caller only sees its own
// deadline. [Client] therefore validates the fields the gateway requires
// before publishing, and applies a default deadline when the context has none.
package ss7client
