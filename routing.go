// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: Apache-2.0

package ss7client

import (
	"context"
	"errors"
	"fmt"
)

// NATS subjects of MAP call routing (3GPP TS 29.002 §10.1, SendRoutingInfo).
// They are not under map.sri.*, the SRI-for-SM subjects, so that NATS
// permissions can grant one without the other: SendRoutingInfo returns the
// IMSI and the serving MSC of any subscriber it is asked about.
const (
	// SubjectRoutingRequest carries SendRoutingInfo requests, application → gateway.
	SubjectRoutingRequest = "map.routing.request"
	// SubjectRoutingResponse carries SendRoutingInfo results and failures, gateway → application.
	SubjectRoutingResponse = "map.routing.response"
)

// MsgTypeMAPRouting requests carry msisdn and, optionally, global_title and
// dpc (the HLR's address). The gateway ignores every other request field —
// opc, ssn, sender, recipient, parameters: the SendRoutingInfo it sends is
// a basic call interrogation whose GMSC address is its own.
const MsgTypeMAPRouting MessageType = "MAP_ROUTING"

// MAP error codes (TS 29.002 MAP-ErrorCodes) that SendRoutingInfo can return
// in addition to those with constants already: unknownSubscriber,
// teleserviceNotProvisioned, callBarred, facilityNotSupported, systemFailure,
// dataMissing, unexpectedDataValue. Error parameters (the absent
// subscriber's reason, the call barring cause) are not passed on.
const (
	MAPErrBearerServiceNotProvisioned int32 = 10
	MAPErrForwardingViolation         int32 = 14
	MAPErrCUGReject                   int32 = 15
	// MAPErrAbsentSubscriber is a call's absentSubscriber, not the
	// short-message MAPErrAbsentSubscriberSM (6).
	MAPErrAbsentSubscriber int32 = 27
	MAPErrNumberChanged    int32 = 44
	// MAPErrBusySubscriber is a call's busySubscriber, not the
	// short-message MAPErrSubscriberBusyForMTSMS (31).
	MAPErrBusySubscriber    int32 = 45
	MAPErrNoSubscriberReply int32 = 46
	MAPErrORNotAllowed      int32 = 48
)

// RoutingInfo is the routing part of a SendRoutingInfo answer (TS 29.002
// SendRoutingInfoRes). The IMSI is in Message.IMSI and, when the HLR returns
// it, the subscriber's location and state in Message.SubscriberInfo. At most
// one of RoamingNumber, Forwarding and CAMEL is set.
type RoutingInfo struct {
	// RoamingNumber is the MSRN to route the call on. From a number
	// portability database it is the routing number followed by the MSISDN,
	// or the MSISDN alone.
	RoamingNumber *AddressString `json:"roaming_number,omitempty"`
	// Forwarding: the call is to be forwarded.
	Forwarding *ForwardingData `json:"forwarding,omitempty"`
	// CAMEL: the subscriber has terminating CAMEL services the GMSC is to
	// invoke before routing. The gateway does not ask for them (it does not
	// offer CAMEL support in the request), so an HLR should not return this.
	CAMEL *CAMELRouting `json:"camel,omitempty"`
	// VMSCAddress is the E.164 address of the MSC serving the subscriber,
	// digits only (as Location.VLRNumber).
	VMSCAddress string `json:"vmsc_address,omitempty"`
	// MSISDN is the subscriber's basic MSISDN, when the HLR returns it.
	MSISDN string `json:"msisdn,omitempty"`
	// ForwardingInterrogationRequired: the HLR asks the GMSC to interrogate
	// it again, for forwarding, if the call is later forwarded.
	ForwardingInterrogationRequired bool `json:"forwarding_interrogation_required,omitempty"`
	// NumberPortabilityStatus is the TS 29.002 NumberPortabilityStatus name:
	// notKnownToBePorted, ownNumberPortedOut or foreignNumberPortedToForeignNetwork.
	NumberPortabilityStatus string `json:"number_portability_status,omitempty"`
	// BasicService is the service the HLR resolved the call to.
	BasicService *BasicService `json:"basic_service,omitempty"`
	// SSCodes are the supplementary services active for the call (TS 29.002
	// SS-Code, one octet each), hex.
	SSCodes []string `json:"ss_codes,omitempty"`
}

// ForwardingData is TS 29.002 ForwardingData.
type ForwardingData struct {
	// ForwardedToNumber is the forwarded-to number (forwardedToNumber, or
	// longForwardedToNumber when the HLR sent the long form). With CAMEL
	// phase 2 an HLR may send a number that is not E.164: Nature and
	// NumberingPlan say how to read it, and non-digit TBCD values come
	// through as '*', '#', 'a', 'b', 'c'.
	ForwardedToNumber *AddressString `json:"forwarded_to_number,omitempty"`
	// ForwardedToSubaddress is the ISDN subaddress, hex.
	ForwardedToSubaddress string `json:"forwarded_to_subaddress,omitempty"`
	// Options are the ForwardingOptions, when present.
	Options *ForwardingOptions `json:"options,omitempty"`
}

// ForwardingOptions is TS 29.002 ForwardingOptions, decoded.
type ForwardingOptions struct {
	NotifyForwardingParty   bool `json:"notify_forwarding_party,omitempty"`
	RedirectingPresentation bool `json:"redirecting_presentation,omitempty"`
	NotifyCallingParty      bool `json:"notify_calling_party,omitempty"`
	// Reason is notReachable, busy, noReply or unconditional.
	Reason string `json:"reason"`
}

// CAMELRouting is TS 29.002 CamelRoutingInfo.
type CAMELRouting struct {
	Forwarding *ForwardingData `json:"forwarding,omitempty"`
	// SubscriptionInfo is the BER encoding of GmscCamelSubscriptionInfo
	// (T-CSI, O-CSI, D-CSI and their criteria), hex, tag included.
	SubscriptionInfo string `json:"subscription_info"`
}

// BasicService is TS 29.002 Ext-BasicServiceCode.
type BasicService struct {
	// Kind is the alternative's ASN.1 name: "ext-BearerService" or
	// "ext-Teleservice".
	Kind string `json:"kind"`
	// Code is the service code (TS 29.002 Ext-BearerServiceCode /
	// Ext-TeleserviceCode, e.g. 11 for telephony), hex.
	Code string `json:"code"`
}

// RoutingRequest is a MAP SendRoutingInfo towards the subscriber's HLR
// (3GPP TS 29.002 §10.1), with the gateway as GMSC.
type RoutingRequest struct {
	MSISDN string // required, E.164 digits
	// HLRGlobalTitle and HLRPointCode address the HLR directly. Without
	// them the gateway routes on the MSISDN as global title (or uses
	// map.hlr_gt / map.hlr_pc when map.sri_routing is "hlr").
	HLRGlobalTitle string
	HLRPointCode   uint32
}

// RoutingResult is a successful SendRoutingInfo answer.
type RoutingResult struct {
	IMSI    string       // the subscriber's IMSI; empty if the HLR left it out
	Routing *RoutingInfo // never nil
	// SubscriberInfo is the location and state the HLR returned, if any.
	SubscriberInfo *SubscriberInfo
	NodeID         string // gateway node that ran the dialogue
	Raw            *Message
}

// SendRoutingInfo asks the subscriber's HLR how to route a basic call to
// req.MSISDN (interrogationType basicCall). The GMSC address the request
// carries is the gateway's own (its cluster GT, else its GT). It needs a
// gateway that serves map.routing.request (0.7.6 and later); an older one
// leaves the request unanswered until the deadline. On a gateway-reported
// failure it returns *GatewayError — MAPErrAbsentSubscriber,
// MAPErrBusySubscriber, MAPErrCallBarred and the like when the HLR refuses
// the call.
func (c *Client) SendRoutingInfo(ctx context.Context, req RoutingRequest) (*RoutingResult, error) {
	if req.MSISDN == "" {
		return nil, fmt.Errorf("%w: SRI: MSISDN required", ErrInvalidRequest)
	}
	if err := checkDigits("SRI", map[string]string{"MSISDN": req.MSISDN, "HLRGlobalTitle": req.HLRGlobalTitle}); err != nil {
		return nil, err
	}
	if err := checkISDNLength("SRI", "MSISDN", req.MSISDN); err != nil {
		return nil, err
	}
	resp, err := c.request(ctx, SubjectRoutingRequest, &Message{
		MessageType: MsgTypeMAPRouting,
		MSISDN:      req.MSISDN,
		GlobalTitle: req.HLRGlobalTitle,
		DPC:         req.HLRPointCode,
	})
	if err != nil {
		return nil, err
	}
	if resp.RoutingInfo == nil {
		return nil, errNoRoutingInfo
	}
	return &RoutingResult{
		IMSI:           resp.IMSI,
		Routing:        resp.RoutingInfo,
		SubscriberInfo: resp.SubscriberInfo,
		NodeID:         resp.NodeID,
		Raw:            resp,
	}, nil
}

var errNoRoutingInfo = errors.New("ss7 client: successful response without routing_info")
