// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: Apache-2.0

package ss7client

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// Authentication and MS purging with the gateway in the HLR's role
// (3GPP TS 29.002 §8.5.2, §8.1.6). Needs gateway 0.7.9 or later.
//
//   - A VLR or SGSN asks for authentication vectors with
//     sendAuthenticationInfo. The application is the AuC: it computes the
//     vectors, the gateway carries them — see ServeAuthentication.
//   - A VLR or SGSN tells the HLR it deleted a subscriber's record with
//     purgeMS: the HLR marks the subscriber purged, so that calls and short
//     messages to it are treated as to an absent subscriber — see
//     ServePurges.
//
// Both are NATS requests, gateway → application, answered with a reply.
//
// The vectors carry key material (Kc, CK, IK, KASME): run NATS with TLS,
// and allow only the gateway to publish on SubjectAuthentication and only
// the AuC to subscribe to it.
const (
	// SubjectAuthentication carries sendAuthenticationInfo requests,
	// gateway → application, as NATS requests (queue group recommended).
	SubjectAuthentication = "map.sai"
	// SubjectPurge carries purgeMS requests, gateway → application, as
	// NATS requests.
	SubjectPurge = "map.purge"
)

const (
	MsgTypeMAPSendAuthenticationInfo MessageType = "MAP_SAI"
	MsgTypeMAPPurgeMS                MessageType = "MAP_PURGE"
)

// The errors an application may refuse sendAuthenticationInfo and purgeMS
// with (TS 29.002 §17.6.4, §17.6.1). purgeMS has no systemFailure.
var (
	authenticationRejectCodes = map[int32]bool{
		MAPErrSystemFailure: true, MAPErrDataMissing: true, MAPErrUnexpectedDataValue: true, MAPErrUnknownSubscriber: true,
	}
	purgeRejectCodes = map[int32]bool{
		MAPErrDataMissing: true, MAPErrUnexpectedDataValue: true, MAPErrUnknownSubscriber: true,
	}
)

// Values of Authentication.RequestingNodeType (TS 29.002
// RequestingNodeType). Empty: the request did not say; the field comes
// after the extension marker, so an older VLR or SGSN leaves it out.
const (
	NodeVLR           = "vlr"
	NodeSGSN          = "sgsn"
	NodeSCSCF         = "s-cscf"
	NodeBSF           = "bsf"
	NodeGANAAAServer  = "gan-aaa-server"
	NodeWLANAAAServer = "wlan-aaa-server"
	NodeMME           = "mme"
	NodeMMESGSN       = "mme-sgsn"
)

// Authentication is a sendAuthenticationInfo's part of a Message on
// SubjectAuthentication: the request, or the application's answer to it.
// Octet strings are hex.
type Authentication struct {
	DialogueID string `json:"dialogue_id,omitempty"`
	// AnswerBy (request): the gateway refuses the request with
	// systemFailure when the answer has not come by then.
	AnswerBy *time.Time `json:"answer_by,omitempty"`

	// The request (TS 29.002 §8.5.2.3).
	RequestedVectors           int    `json:"requested_vectors,omitempty"`
	RequestingNodeType         string `json:"requesting_node_type,omitempty"`
	RequestingPLMNID           string `json:"requesting_plmn_id,omitempty"` // 3 octets
	ImmediateResponsePreferred bool   `json:"immediate_response_preferred,omitempty"`
	SegmentationProhibited     bool   `json:"segmentation_prohibited,omitempty"`
	// Resynchronisation: the UE found the sequence number out of range
	// (TS 33.102 §6.3.5).
	Resynchronisation          *Resynchronisation `json:"resynchronisation,omitempty"`
	AdditionalVectors          int                `json:"additional_vectors,omitempty"`
	AdditionalVectorsAreForEPS bool               `json:"additional_vectors_are_for_eps,omitempty"`
	UEUsageTypeRequested       bool               `json:"ue_usage_type_requested,omitempty"`
	// CallingGT: the calling party's global title digits.
	CallingGT string `json:"calling_gt,omitempty"`

	// The answer: vectors, or Error.
	Triplets    []AuthenticationTriplet    `json:"triplets,omitempty"`
	Quintuplets []AuthenticationQuintuplet `json:"quintuplets,omitempty"`
	EPSVectors  []EPSVector                `json:"eps_vectors,omitempty"`
	UEUsageType string                     `json:"ue_usage_type,omitempty"` // 4 octets
	Error       int32                      `json:"error,omitempty"`
	ErrorCause  string                     `json:"error_cause,omitempty"` // a Cause* qualifying Error
	Detail      string                     `json:"detail,omitempty"`
}

// Resynchronisation is re-synchronisationInfo: RAND 16 octets, AUTS 14.
type Resynchronisation struct {
	RAND string `json:"rand"`
	AUTS string `json:"auts"`
}

// AuthenticationTriplet is a GSM vector: RAND 16 octets, SRES 4, Kc 8.
type AuthenticationTriplet struct {
	RAND string `json:"rand"`
	SRES string `json:"sres"`
	Kc   string `json:"kc"`
}

// AuthenticationQuintuplet is a UMTS vector: RAND 16 octets, XRES 4..16,
// CK, IK and AUTN 16.
type AuthenticationQuintuplet struct {
	RAND string `json:"rand"`
	XRES string `json:"xres"`
	CK   string `json:"ck"`
	IK   string `json:"ik"`
	AUTN string `json:"autn"`
}

// EPSVector is an EPS vector (EPC-AV): RAND 16 octets, XRES 4..16, AUTN 16,
// KASME 32.
type EPSVector struct {
	RAND  string `json:"rand"`
	XRES  string `json:"xres"`
	AUTN  string `json:"autn"`
	KASME string `json:"kasme"`
}

// Purge is a purgeMS's part of a Message on SubjectPurge: the request, or
// the application's answer to it.
type Purge struct {
	DialogueID string     `json:"dialogue_id,omitempty"`
	AnswerBy   *time.Time `json:"answer_by,omitempty"`

	// The request: the VLR's number when a VLR sends it, the SGSN's when
	// an SGSN does.
	VLRNumber  *AddressString `json:"vlr_number,omitempty"`
	SGSNNumber *AddressString `json:"sgsn_number,omitempty"`
	CallingGT  string         `json:"calling_gt,omitempty"`

	// The answer: what to freeze (TS 29.002 §8.1.6.3 — the TMSI when the
	// VLR number is the one stored, the P-TMSI likewise for the SGSN), or
	// Error.
	FreezeTMSI  bool   `json:"freeze_tmsi,omitempty"`
	FreezePTMSI bool   `json:"freeze_ptmsi,omitempty"`
	FreezeMTMSI bool   `json:"freeze_mtmsi,omitempty"`
	Error       int32  `json:"error,omitempty"`
	ErrorCause  string `json:"error_cause,omitempty"` // a Cause* qualifying Error
	Detail      string `json:"detail,omitempty"`
}

// ----- authentication -------------------------------------------------------

// AuthenticationRequest is a sendAuthenticationInfo as the handler gets it.
type AuthenticationRequest struct {
	DialogueID string
	IMSI       string
	Request    *Authentication
	NodeID     string
	Raw        *Message
}

// AuthenticationAnswer is the application's answer: vectors, or Reject.
// Triplets for a GSM subscriber, quintuplets for a UMTS one (never both),
// EPS vectors for an MME; no more of each kind than
// Request.VectorLimits allows. No vectors and no
// Reject is the empty response: the HLR has none to give (TS 29.002
// §8.5.2.1).
type AuthenticationAnswer struct {
	Triplets    []AuthenticationTriplet
	Quintuplets []AuthenticationQuintuplet
	EPSVectors  []EPSVector
	// UEUsageType: only when Request.UEUsageTypeRequested.
	UEUsageType string
	// Reject refuses the request with one of sendAuthenticationInfo's
	// errors: MAPErrSystemFailure, MAPErrDataMissing,
	// MAPErrUnexpectedDataValue or MAPErrUnknownSubscriber.
	Reject int32
	// RejectCause qualifies a MAPErrUnknownSubscriber: CauseIMSIUnknown,
	// or CauseGPRSEPSSubscriptionUnknown when an SGSN asks for a subscriber
	// without GPRS. Optional.
	RejectCause string
}

// ServeAuthentication makes the application the AuC behind the gateway's
// HLR (TS 29.002 §8.5.2): each sendAuthenticationInfo a VLR or SGSN sends
// is a request to handle, whose answer goes back as the result or error.
//
// The gateway divides vectors that do not fit one message into several
// results, as TS 29.002 Annex C recommends for this operation, unless the
// request has SegmentationProhibited: then it sends as many as fit one
// message and drops the rest. A request with Resynchronisation asks the
// AuC to re-synchronise the sequence number before computing vectors.
//
// handle runs on its own goroutine with a context ending at AnswerBy. An
// answer that breaks the rules above is sent as MAPErrSystemFailure, and
// the error reported to WithDecodeErrorHandler. With queue non-empty the
// subscription is in that queue group, so instances of the application
// share the requests.
func (c *Client) ServeAuthentication(queue string, handle func(ctx context.Context, r *AuthenticationRequest) AuthenticationAnswer) (*nats.Subscription, error) {
	return c.serveRequests(SubjectAuthentication, queue, func(msg *Message) error {
		if msg.Authentication == nil {
			return errors.New("no authentication")
		}
		return nil
	}, func(m *nats.Msg, msg *Message) {
		a := msg.Authentication
		ctx, cancel := answerContext(a.AnswerBy)
		defer cancel()
		answer := handle(ctx, &AuthenticationRequest{DialogueID: a.DialogueID, IMSI: msg.IMSI, Request: a, NodeID: msg.NodeID, Raw: msg})
		reply := &Authentication{DialogueID: a.DialogueID}
		switch err := answer.check(a); {
		case err != nil:
			reply.Error, reply.Detail = MAPErrSystemFailure, err.Error()
			c.reportServeError(m.Subject, err)
		case answer.Reject != 0:
			reply.Error, reply.ErrorCause = answer.Reject, answer.RejectCause
		default:
			reply.Triplets, reply.Quintuplets, reply.EPSVectors = answer.Triplets, answer.Quintuplets, answer.EPSVectors
			reply.UEUsageType = answer.UEUsageType
		}
		c.reply(m, &Message{CorrelationID: msg.CorrelationID, MessageType: MsgTypeMAPSendAuthenticationInfo, Authentication: reply})
	})
}

// VectorLimits are how many vectors of each kind an answer to r may
// carry (TS 29.002 §8.5.2.1, §8.5.2.3): an MME gets EPS vectors only, the
// number it asked for; a combined MME/SGSN gets the number it asked for of
// the kind for immediate use and AdditionalVectors of the other kind — EPS
// when AdditionalVectorsAreForEPS, else triplets or quintuplets; any other
// node, or a request without a node type, gets triplets or quintuplets
// only.
func (r *Authentication) VectorLimits() (setList, eps int) {
	switch r.RequestingNodeType {
	case NodeMME:
		return 0, r.RequestedVectors
	case NodeMMESGSN:
		if r.AdditionalVectorsAreForEPS {
			return r.RequestedVectors, r.AdditionalVectors
		}
		return r.AdditionalVectors, r.RequestedVectors
	}
	return r.RequestedVectors, 0
}

// check checks what the gateway will check.
func (a *AuthenticationAnswer) check(req *Authentication) error {
	if a.Reject != 0 {
		if len(a.Triplets)+len(a.Quintuplets)+len(a.EPSVectors) > 0 || a.UEUsageType != "" {
			return errors.New("authentication answer: Reject with vectors")
		}
		if !authenticationRejectCodes[a.Reject] {
			return fmt.Errorf("authentication answer: %d is not an error sendAuthenticationInfo allows", a.Reject)
		}
		if err := checkRejectCause(a.Reject, a.RejectCause); err != nil {
			return fmt.Errorf("authentication answer: %w", err)
		}
		return nil
	}
	if a.RejectCause != "" {
		return errors.New("authentication answer: RejectCause without Reject")
	}
	if len(a.Triplets) > 0 && len(a.Quintuplets) > 0 {
		return errors.New("authentication answer: triplets and quintuplets, never both")
	}
	set, eps := req.VectorLimits()
	if n := len(a.Triplets) + len(a.Quintuplets); n > set {
		return fmt.Errorf("authentication answer: %d triplets or quintuplets, the request allows %d", n, set)
	}
	if n := len(a.EPSVectors); n > eps {
		return fmt.Errorf("authentication answer: %d EPS vectors, the request allows %d", n, eps)
	}
	for _, l := range []int{len(a.Triplets), len(a.Quintuplets), len(a.EPSVectors)} {
		if l > 5 {
			return fmt.Errorf("authentication answer: a list of %d vectors, at most 5", l)
		}
	}
	type field struct {
		name, value string
		min, max    int
	}
	var fields []field
	for i, v := range a.Triplets {
		p := fmt.Sprintf("triplet %d ", i)
		fields = append(fields, field{p + "RAND", v.RAND, 16, 16}, field{p + "SRES", v.SRES, 4, 4}, field{p + "Kc", v.Kc, 8, 8})
	}
	for i, v := range a.Quintuplets {
		p := fmt.Sprintf("quintuplet %d ", i)
		fields = append(fields, field{p + "RAND", v.RAND, 16, 16}, field{p + "XRES", v.XRES, 4, 16},
			field{p + "CK", v.CK, 16, 16}, field{p + "IK", v.IK, 16, 16}, field{p + "AUTN", v.AUTN, 16, 16})
	}
	for i, v := range a.EPSVectors {
		p := fmt.Sprintf("EPS vector %d ", i)
		fields = append(fields, field{p + "RAND", v.RAND, 16, 16}, field{p + "XRES", v.XRES, 4, 16},
			field{p + "AUTN", v.AUTN, 16, 16}, field{p + "KASME", v.KASME, 32, 32})
	}
	for _, f := range fields {
		if b, err := hex.DecodeString(f.value); err != nil || len(b) < f.min || len(b) > f.max {
			return fmt.Errorf("authentication answer: %s is not hex of %d..%d octets", f.name, f.min, f.max)
		}
	}
	if a.UEUsageType != "" {
		if !req.UEUsageTypeRequested {
			return errors.New("authentication answer: a UE usage type the request did not ask for")
		}
		if b, err := hex.DecodeString(a.UEUsageType); err != nil || len(b) != 4 {
			return errors.New("authentication answer: UEUsageType is not hex of 4 octets")
		}
	}
	return nil
}

// ----- purging --------------------------------------------------------------

// PurgeRequest is a purgeMS as the handler gets it.
type PurgeRequest struct {
	DialogueID string
	IMSI       string
	Request    *Purge
	NodeID     string
	Raw        *Message
}

// PurgeAnswer is the application's answer: what to freeze, or Reject.
type PurgeAnswer struct {
	FreezeTMSI, FreezePTMSI, FreezeMTMSI bool
	// Reject refuses the request with one of purgeMS's errors:
	// MAPErrDataMissing, MAPErrUnexpectedDataValue or
	// MAPErrUnknownSubscriber.
	Reject int32
	// RejectCause qualifies a MAPErrUnknownSubscriber: CauseIMSIUnknown or
	// CauseGPRSEPSSubscriptionUnknown. Optional.
	RejectCause string
}

// ServePurges makes the application the HLR's subscriber records for
// purgeMS (TS 29.002 §8.1.6, §19.1.4): mark the subscriber purged, so that
// routing information for calls and short messages to it reports it
// absent, and answer what the sender is to freeze.
//
// handle runs on its own goroutine with a context ending at AnswerBy. When
// no answer comes in time, or one breaks the rules (purgeMS has no
// systemFailure), the gateway aborts the dialogue and the sender retries
// later. With queue non-empty the subscription is in that queue group.
func (c *Client) ServePurges(queue string, handle func(ctx context.Context, r *PurgeRequest) PurgeAnswer) (*nats.Subscription, error) {
	return c.serveRequests(SubjectPurge, queue, func(msg *Message) error {
		if msg.Purge == nil {
			return errors.New("no purge")
		}
		return nil
	}, func(m *nats.Msg, msg *Message) {
		p := msg.Purge
		ctx, cancel := answerContext(p.AnswerBy)
		defer cancel()
		answer := handle(ctx, &PurgeRequest{DialogueID: p.DialogueID, IMSI: msg.IMSI, Request: p, NodeID: msg.NodeID, Raw: msg})
		reply := &Purge{DialogueID: p.DialogueID}
		causeErr := checkRejectCause(answer.Reject, answer.RejectCause)
		switch {
		case answer.Reject == 0 && answer.RejectCause == "":
			reply.FreezeTMSI, reply.FreezePTMSI, reply.FreezeMTMSI = answer.FreezeTMSI, answer.FreezePTMSI, answer.FreezeMTMSI
		case purgeRejectCodes[answer.Reject] && causeErr == nil && !answer.FreezeTMSI && !answer.FreezePTMSI && !answer.FreezeMTMSI:
			reply.Error, reply.ErrorCause = answer.Reject, answer.RejectCause
		default:
			// systemFailure, which purgeMS does not have: the gateway
			// aborts the dialogue.
			err := fmt.Errorf("purge answer: Reject %d (cause %q) is not an error purgeMS allows, or comes with freezes", answer.Reject, answer.RejectCause)
			reply.Error, reply.Detail = MAPErrSystemFailure, err.Error()
			c.reportServeError(m.Subject, err)
		}
		c.reply(m, &Message{CorrelationID: msg.CorrelationID, MessageType: MsgTypeMAPPurgeMS, Purge: reply})
	})
}

// ----- serving requests -----------------------------------------------------

// serveRequests subscribes to subject, the gateway's requests, and runs
// serve for each on its own goroutine.
func (c *Client) serveRequests(subject, queue string, check func(*Message) error, serve func(*nats.Msg, *Message)) (*nats.Subscription, error) {
	cb := func(m *nats.Msg) {
		var msg Message
		err := json.Unmarshal(m.Data, &msg)
		if err == nil {
			err = check(&msg)
		}
		if err != nil {
			c.reportServeError(m.Subject, err)
			return
		}
		go serve(m, &msg)
	}
	if queue == "" {
		return c.nc.Subscribe(subject, cb)
	}
	return c.nc.QueueSubscribe(subject, queue, cb)
}

func answerContext(by *time.Time) (context.Context, context.CancelFunc) {
	if by == nil {
		return context.WithCancel(context.Background())
	}
	return context.WithDeadline(context.Background(), *by)
}

// reply answers a request with msg, its envelope filled in here.
func (c *Client) reply(m *nats.Msg, msg *Message) {
	if m.Reply == "" {
		return
	}
	msg.MessageID, msg.Direction, msg.Timestamp = uuid.NewString(), DirectionInbound, time.Now().UTC()
	data, err := json.Marshal(msg)
	if err == nil {
		err = m.Respond(data)
	}
	if err != nil {
		c.reportServeError(m.Subject, fmt.Errorf("answer not sent: %w", err))
	}
}

func (c *Client) reportServeError(subject string, err error) {
	if c.onDecodeError != nil {
		c.onDecodeError(subject, nil, err)
	}
}
