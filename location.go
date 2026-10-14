// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: Apache-2.0

package ss7client

import (
	"context"
	"errors"
	"fmt"
)

// NATS subjects of the MAP location interface (3GPP TS 29.002 §8.11).
const (
	// SubjectATIRequest carries AnyTimeInterrogation requests, application → gateway.
	SubjectATIRequest = "map.ati.request"
	// SubjectATIResponse carries AnyTimeInterrogation results and failures, gateway → application.
	SubjectATIResponse = "map.ati.response"
	// SubjectPSIRequest carries ProvideSubscriberInfo requests, application → gateway.
	SubjectPSIRequest = "map.psi.request"
	// SubjectPSIResponse carries ProvideSubscriberInfo results and failures, gateway → application.
	SubjectPSIResponse = "map.psi.response"
)

const (
	MsgTypeMAPATI MessageType = "MAP_ATI"
	MsgTypeMAPPSI MessageType = "MAP_PSI"
)

// Keys of Message.Parameters for the location interface.
const (
	// ParamVLRNumber (request, PSI): the address of the VLR serving the
	// subscriber, as a digit string or an [AddressString] object.
	ParamVLRNumber = "vlr_number"
)

// MAPErrATINotAllowed is ati-NotAllowed (TS 29.002 MAP-ErrorCodes): the HLR
// does not accept AnyTimeInterrogation from this gsmSCF-Address. The other
// errors of ATI and PSI — systemFailure, dataMissing, unexpectedDataValue,
// unknownSubscriber — have MAPErr* constants already.
const MAPErrATINotAllowed int32 = 49

// Domains of RequestedInfo.Domain (TS 29.002 DomainType).
const (
	DomainCS = "cs"
	DomainPS = "ps"
)

// RequestedInfo selects what an AnyTimeInterrogation or ProvideSubscriberInfo
// asks for (TS 29.002 RequestedInfo). At least one item must be set.
type RequestedInfo struct {
	// Location asks for the location information of Domain.
	Location bool `json:"location,omitempty"`
	// CurrentLocation asks the serving node to page the subscriber for an
	// up-to-date location instead of the last known one. Requires Location.
	CurrentLocation bool `json:"current_location,omitempty"`
	// EPSLocation tells the HLR that the E-UTRAN location may be returned,
	// in Location.EPS or in SubscriberInfo.LocationEPS. Requires Location.
	EPSLocation bool `json:"eps_location,omitempty"`
	// State asks for the subscriber state of Domain.
	State bool `json:"state,omitempty"`
	// Domain is DomainCS (the default when empty) or DomainPS.
	Domain string `json:"domain,omitempty"`
	// IMEI asks for the equipment identity.
	IMEI bool `json:"imei,omitempty"`
	// MSClassmark asks for MS Classmark 2, returned for the CS domain.
	MSClassmark bool `json:"ms_classmark,omitempty"`
	// MNP asks for number portability information. AnyTimeInterrogation only.
	MNP bool `json:"mnp,omitempty"`
}

func (r RequestedInfo) any() bool {
	return r.Location || r.State || r.IMEI || r.MSClassmark || r.MNP
}

func (r RequestedInfo) validate(op string) error {
	switch {
	case !r.any():
		return fmt.Errorf("%w: %s: RequestedInfo selects nothing", ErrInvalidRequest, op)
	case (r.CurrentLocation || r.EPSLocation) && !r.Location:
		return fmt.Errorf("%w: %s: CurrentLocation and EPSLocation require Location", ErrInvalidRequest, op)
	case r.Domain != "" && r.Domain != DomainCS && r.Domain != DomainPS:
		return fmt.Errorf("%w: %s: Domain %q is not %q or %q", ErrInvalidRequest, op, r.Domain, DomainCS, DomainPS)
	}
	return nil
}

// SubscriberInfo is the answer to an AnyTimeInterrogation or a
// ProvideSubscriberInfo (TS 29.002 SubscriberInfo): only what was requested
// and what the network knows is present. Octet strings whose layout another
// specification defines are passed through as lowercase hex.
type SubscriberInfo struct {
	// Location is the circuit-switched location (MSC/VLR).
	Location *Location `json:"location,omitempty"`
	// State is the circuit-switched subscriber state.
	State *SubscriberState `json:"state,omitempty"`
	// LocationGPRS is the packet-switched location (SGSN).
	LocationGPRS *Location `json:"location_gprs,omitempty"`
	// PSState is the packet-switched subscriber state.
	PSState *SubscriberState `json:"ps_state,omitempty"`
	// EPSState is the EPS subscriber state.
	EPSState *SubscriberState `json:"eps_state,omitempty"`
	// LocationEPS is the EPS location returned outside Location.
	LocationEPS *LocationEPS `json:"location_eps,omitempty"`
	// IMEI digits (TS 23.003), 15 or 16 of them.
	IMEI string `json:"imei,omitempty"`
	// MSClassmark2 is the value part of the MS Classmark 2 IE (TS 24.008), hex.
	MSClassmark2 string `json:"ms_classmark2,omitempty"`
	// MNP is the number portability information.
	MNP *MNPInfo `json:"mnp,omitempty"`
}

// Location is TS 29.002 LocationInformation (CS) or LocationInformationGPRS (PS).
type Location struct {
	// AgeMinutes is the time since the last contact with the subscriber:
	// 0 = in contact now, 32767 = at least that old.
	AgeMinutes *int `json:"age_minutes,omitempty"`
	// VLRNumber and MSCNumber (CS) or SGSNNumber (PS): E.164 digits.
	VLRNumber  string `json:"vlr_number,omitempty"`
	MSCNumber  string `json:"msc_number,omitempty"`
	SGSNNumber string `json:"sgsn_number,omitempty"`
	// At most one of CellGlobalID, ServiceAreaID and LocationArea is set.
	CellGlobalID  *CellGlobalID  `json:"cell_global_id,omitempty"`
	ServiceAreaID *ServiceAreaID `json:"service_area_id,omitempty"`
	LocationArea  *LocationArea  `json:"location_area,omitempty"`
	// RoutingAreaID is the RAI value part (TS 29.060), hex. PS only.
	RoutingAreaID string `json:"routing_area_id,omitempty"`
	// LocationNumber (ITU-T Q.763), hex. CS only.
	LocationNumber string `json:"location_number,omitempty"`
	// GeographicalInformation (TS 23.032 ellipsoid point with uncertainty
	// circle, 8 octets) and GeodeticInformation (Q.763, 10 octets), hex.
	GeographicalInformation string `json:"geographical_information,omitempty"`
	GeodeticInformation     string `json:"geodetic_information,omitempty"`
	// CurrentLocationRetrieved: the location was obtained by paging just now.
	CurrentLocationRetrieved bool `json:"current_location_retrieved,omitempty"`
	// EPS is the E-UTRAN location, when the HLR returned it within the CS
	// location. CS only.
	EPS *LocationEPS `json:"eps,omitempty"`
}

// CellGlobalID is a cell global identity or service area identity (TS 23.003).
type CellGlobalID struct {
	MCC string `json:"mcc"`
	MNC string `json:"mnc"`
	LAC uint16 `json:"lac"`
	CI  uint16 `json:"ci"`
}

// ServiceAreaID is a service area identity (TS 23.003): SAC is the service
// area code.
type ServiceAreaID struct {
	MCC string `json:"mcc"`
	MNC string `json:"mnc"`
	LAC uint16 `json:"lac"`
	SAC uint16 `json:"sac"`
}

// LocationArea is a location area identity (TS 23.003).
type LocationArea struct {
	MCC string `json:"mcc"`
	MNC string `json:"mnc"`
	LAC uint16 `json:"lac"`
}

// LocationEPS is TS 29.002 LocationInformationEPS.
type LocationEPS struct {
	// ECGI and TAI as coded in TS 29.118, hex.
	ECGI string `json:"ecgi,omitempty"`
	TAI  string `json:"tai,omitempty"`
	// MMEName is the serving MME's Diameter identity.
	MMEName                  string `json:"mme_name,omitempty"`
	AgeMinutes               *int   `json:"age_minutes,omitempty"`
	GeographicalInformation  string `json:"geographical_information,omitempty"`
	GeodeticInformation      string `json:"geodetic_information,omitempty"`
	CurrentLocationRetrieved bool   `json:"current_location_retrieved,omitempty"`
}

// SubscriberState is TS 29.002 SubscriberState (CS) or PS-SubscriberState
// (PS, EPS). State is the ASN.1 name of the alternative:
//
//   - CS: assumedIdle, camelBusy, netDetNotReachable, notProvidedFromVLR;
//   - PS/EPS: notProvidedFromSGSNorMME, ps-Detached,
//     ps-AttachedNotReachableForPaging, ps-AttachedReachableForPaging,
//     ps-PDP-ActiveNotReachableForPaging, ps-PDP-ActiveReachableForPaging,
//     netDetNotReachable.
//
// NotReachableReason accompanies netDetNotReachable: msPurged, imsiDetached,
// restrictedArea or notRegistered.
type SubscriberState struct {
	State              string `json:"state"`
	NotReachableReason string `json:"not_reachable_reason,omitempty"`
}

// MNPInfo is TS 29.002 MNPInfoRes.
type MNPInfo struct {
	RouteingNumber string `json:"routeing_number,omitempty"`
	IMSI           string `json:"imsi,omitempty"`
	MSISDN         string `json:"msisdn,omitempty"`
	// Status is the NumberPortabilityStatus name: notKnownToBePorted,
	// ownNumberPortedOut, foreignNumberPortedToForeignNetwork,
	// ownNumberNotPortedOut, foreignNumberPortedIn.
	Status string `json:"status,omitempty"`
}

// ATIRequest is a MAP AnyTimeInterrogation towards the subscriber's HLR
// (3GPP TS 29.002 §8.11.1).
type ATIRequest struct {
	// Exactly one of MSISDN and IMSI (digits) identifies the subscriber.
	MSISDN string
	IMSI   string
	// Requested selects what to ask for.
	Requested RequestedInfo
	// HLRGlobalTitle and HLRPointCode address the HLR directly. Without
	// them the gateway routes on the MSISDN as global title (or uses
	// map.hlr_gt / map.hlr_pc when map.sri_routing is "hlr"); a request by
	// IMSI therefore needs one of them or map.sri_routing "hlr".
	HLRGlobalTitle string
	HLRPointCode   uint32
}

// PSIRequest is a MAP ProvideSubscriberInfo towards the VLR serving the
// subscriber (3GPP TS 29.002 §8.11.2).
type PSIRequest struct {
	IMSI string // required
	// VLRNumber is the serving VLR's address (required). For a combined
	// MSC/VLR it is normally the network node number SendRoutingInfoForSM
	// returned.
	VLRNumber AddressString
	// Requested selects what to ask for. MNP is not available here.
	Requested RequestedInfo
	// VLRPointCode routes on point code instead of the VLR number GT.
	VLRPointCode uint32
}

// SubscriberInfoResult is a successful AnyTimeInterrogation or
// ProvideSubscriberInfo answer.
type SubscriberInfoResult struct {
	Info   *SubscriberInfo // never nil
	NodeID string          // gateway node that ran the dialogue
	Raw    *Message
}

// AnyTimeInterrogation asks the subscriber's HLR for the information in
// req.Requested. The gsmSCF-Address the HLR screens the request on is the
// gateway's own (map.gsmscf_address, else its cluster GT, else its GT); a
// request cannot choose it, so a NATS publisher cannot present itself as
// another gsmSCF. On a gateway-reported failure it returns *GatewayError —
// with MAPErrATINotAllowed when the HLR refuses this gsmSCF.
func (c *Client) AnyTimeInterrogation(ctx context.Context, req ATIRequest) (*SubscriberInfoResult, error) {
	if (req.MSISDN == "") == (req.IMSI == "") {
		return nil, fmt.Errorf("%w: ATI: exactly one of MSISDN and IMSI required", ErrInvalidRequest)
	}
	if err := checkDigits("ATI", map[string]string{
		"MSISDN": req.MSISDN, "IMSI": req.IMSI, "HLRGlobalTitle": req.HLRGlobalTitle,
	}); err != nil {
		return nil, err
	}
	if err := req.Requested.validate("ATI"); err != nil {
		return nil, err
	}
	requested := req.Requested
	msg := &Message{
		MessageType:   MsgTypeMAPATI,
		MSISDN:        req.MSISDN,
		IMSI:          req.IMSI,
		GlobalTitle:   req.HLRGlobalTitle,
		DPC:           req.HLRPointCode,
		RequestedInfo: &requested,
	}
	resp, err := c.request(ctx, SubjectATIRequest, msg)
	if err != nil {
		return nil, err
	}
	return subscriberInfoResult(resp)
}

// ProvideSubscriberInfo asks the VLR serving the subscriber for the
// information in req.Requested. On a gateway-reported failure it returns
// *GatewayError.
func (c *Client) ProvideSubscriberInfo(ctx context.Context, req PSIRequest) (*SubscriberInfoResult, error) {
	switch {
	case req.IMSI == "":
		return nil, fmt.Errorf("%w: PSI: IMSI required", ErrInvalidRequest)
	case req.VLRNumber.Digits == "":
		return nil, fmt.Errorf("%w: PSI: VLRNumber required", ErrInvalidRequest)
	case req.Requested.MNP:
		return nil, fmt.Errorf("%w: PSI: MNP is available through AnyTimeInterrogation only", ErrInvalidRequest)
	}
	if err := checkDigits("PSI", map[string]string{"IMSI": req.IMSI, "VLRNumber": req.VLRNumber.Digits}); err != nil {
		return nil, err
	}
	if err := req.Requested.validate("PSI"); err != nil {
		return nil, err
	}
	requested := req.Requested
	resp, err := c.request(ctx, SubjectPSIRequest, &Message{
		MessageType:   MsgTypeMAPPSI,
		IMSI:          req.IMSI,
		DPC:           req.VLRPointCode,
		RequestedInfo: &requested,
		Parameters:    map[string]interface{}{ParamVLRNumber: req.VLRNumber},
	})
	if err != nil {
		return nil, err
	}
	return subscriberInfoResult(resp)
}

// checkDigits rejects identities and addresses that are not decimal digits:
// they become global titles and TBCD strings on the wire. Empty values are
// left to the required-field checks.
func checkDigits(op string, fields map[string]string) error {
	for name, v := range fields {
		for _, c := range v {
			if c < '0' || c > '9' {
				return fmt.Errorf("%w: %s: %s %q is not decimal digits", ErrInvalidRequest, op, name, v)
			}
		}
	}
	return nil
}

var errNoSubscriberInfo = errors.New("ss7 client: successful response without subscriber_info")

func subscriberInfoResult(resp *Message) (*SubscriberInfoResult, error) {
	if resp.SubscriberInfo == nil {
		return nil, errNoSubscriberInfo
	}
	return &SubscriberInfoResult{Info: resp.SubscriberInfo, NodeID: resp.NodeID, Raw: resp}, nil
}
