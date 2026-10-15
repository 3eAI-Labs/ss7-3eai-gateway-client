// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: Apache-2.0

package ss7client

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// Location management with the gateway in the HLR's role (3GPP TS 29.002
// §8.1, §8.8.1, §10.2), for an application that is the HLR's location
// management (TS 23.012): it holds the subscribers and decides, the gateway
// speaks MAP to the VLRs. Needs gateway 0.7.8 or later.
//
//   - Location updates: a VLR registers a subscriber with updateLocation.
//     The gateway asks the application what to do, one turn at a time, as
//     the MAP process in the HLR does (TS 29.002 figure 19.1.1/13) — see
//     ServeLocationUpdates.
//   - CancelLocation tells a VLR to forget a subscriber (it moved to another
//     VLR, or the subscription was withdrawn).
//   - ProvideRoamingNumber asks the VLR serving a subscriber for a roaming
//     number (MSRN) to route a call on.
//   - InsertSubscriberData sends changed subscriber data to the VLR serving
//     a subscriber.
//
// The three requests go to the VLR named by its number (ParamVLRNumber's
// digits, as for ProvideSubscriberInfo) as global title with SSN 7, or to
// DPC when given; the gateway answers from SSN 6, so sccp.local_subsystems
// must include it.
const (
	// SubjectLocationUpdate carries the turns of location updates,
	// gateway → application, as NATS requests: the application answers
	// each with a reply (queue group recommended).
	SubjectLocationUpdate = "map.ul"
	// SubjectCancelLocationRequest carries CancelLocation requests,
	// application → gateway.
	SubjectCancelLocationRequest = "map.cl.request"
	// SubjectCancelLocationResponse carries their results and failures.
	SubjectCancelLocationResponse = "map.cl.response"
	// SubjectPRNRequest carries ProvideRoamingNumber requests.
	SubjectPRNRequest = "map.prn.request"
	// SubjectPRNResponse carries their results and failures.
	SubjectPRNResponse = "map.prn.response"
	// SubjectISDRequest carries stand-alone InsertSubscriberData requests.
	SubjectISDRequest = "map.isd.request"
	// SubjectISDResponse carries their results and failures.
	SubjectISDResponse = "map.isd.response"
)

const (
	MsgTypeMAPUpdateLocation       MessageType = "MAP_UL"
	MsgTypeMAPCancelLocation       MessageType = "MAP_CL"
	MsgTypeMAPProvideRoamingNumber MessageType = "MAP_PRN"
	MsgTypeMAPInsertSubscriberData MessageType = "MAP_ISD"
)

// MAP error codes of the mobility operations not defined before.
// updateLocation is answered with MAPErrSystemFailure, MAPErrDataMissing,
// MAPErrUnexpectedDataValue, MAPErrUnknownSubscriber or
// MAPErrRoamingNotAllowed. A VLR answers cancelLocation with
// MAPErrDataMissing or MAPErrUnexpectedDataValue; insertSubscriberData with
// those or MAPErrUnidentifiedSubscriber; provideRoamingNumber with
// MAPErrSystemFailure, MAPErrDataMissing, MAPErrUnexpectedDataValue,
// MAPErrFacilityNotSupported, MAPErrORNotAllowed, MAPErrAbsentSubscriber
// or MAPErrNoRoamingNumberAvailable.
const (
	MAPErrRoamingNotAllowed        int32 = 8
	MAPErrNoRoamingNumberAvailable int32 = 39
)

// The errors an application may refuse a location update with (TS 29.002
// updateLocation).
var updateLocationRejectCodes = map[int32]bool{
	MAPErrSystemFailure: true, MAPErrDataMissing: true, MAPErrUnexpectedDataValue: true,
	MAPErrUnknownSubscriber: true, MAPErrRoamingNotAllowed: true,
}

// Values of LocationUpdate.Action.
const (
	// Turns, gateway → application.
	ULUpdateLocation = "update_location" // the VLR's updateLocation: what to do?
	ULInsertResult   = "insert_result"   // what became of the subscriber data sent
	ULEnd            = "end"             // the location update ended without the application's answer
	// Answers, application → gateway.
	ULInsert = "insert" // send this subscriber data (insertSubscriberData)
	ULAccept = "accept" // the location update succeeded: updateLocation's result
	ULReject = "reject" // it failed: updateLocation's error Error
)

// LocationUpdate is a location update's part of a Message on
// SubjectLocationUpdate: a turn, or the application's answer to one.
type LocationUpdate struct {
	// DialogueID is the same in every turn of one location update. Opaque,
	// at most 64 characters.
	DialogueID string `json:"dialogue_id,omitempty"`
	Action     string `json:"action"`
	// AnswerBy is when the gateway stops waiting for the answer to a turn;
	// it then refuses the location update with systemFailure.
	AnswerBy *time.Time `json:"answer_by,omitempty"`

	// The update_location turn: what the VLR sent (UpdateLocationArg). The
	// IMSI is in Message.IMSI.
	MSCNumber     *AddressString `json:"msc_number,omitempty"`
	VLRNumber     *AddressString `json:"vlr_number,omitempty"`
	LMSI          string         `json:"lmsi,omitempty"` // hex, 4 octets
	VLRCapability *VLRCapability `json:"vlr_capability,omitempty"`
	// IMEISV is the terminal's IMEISV (16 digits), when the VLR sent it.
	IMEISV                      string `json:"imeisv,omitempty"`
	SkipSubscriberDataUpdate    bool   `json:"skip_subscriber_data_update,omitempty"`
	RestorationIndicator        bool   `json:"restoration_indicator,omitempty"`
	InformPreviousNetworkEntity bool   `json:"inform_previous_network_entity,omitempty"`
	CSLCSNotSupportedByUE       bool   `json:"cs_lcs_not_supported_by_ue,omitempty"`
	// CallingGT is the global title the VLR's request came from (SCCP
	// calling address): GSMA FS.11 screening compares it with VLRNumber.
	CallingGT string `json:"calling_gt,omitempty"`

	// Error is the MAP error of a reject answer.
	Error int32 `json:"error,omitempty"`
	// Detail says why, in an end turn or in an insert_result turn that
	// reports a failure.
	Detail string `json:"detail,omitempty"`
}

// VLRCapability is TS 29.002 VLR-Capability.
type VLRCapability struct {
	// SupportedCAMELPhases are the CAMEL phase numbers, 1 to 4.
	SupportedCAMELPhases []int `json:"supported_camel_phases,omitempty"`
	// OfferedCAMEL4CSIs are the ASN.1 names: o-csi, d-csi, vt-csi, t-csi,
	// mt-sms-csi, mg-csi, psi-enhancements.
	OfferedCAMEL4CSIs []string `json:"offered_camel4_csis,omitempty"`
	// SupportedRATTypes are the ASN.1 names: utran, geran, gan,
	// i-hspa-evolution, e-utran, nb-iot.
	SupportedRATTypes []string `json:"supported_rat_types,omitempty"`
	// SupportedLCSCapabilitySets are the set numbers, 1 to 5.
	SupportedLCSCapabilitySets   []int `json:"supported_lcs_capability_sets,omitempty"`
	LongFTNSupported             bool  `json:"long_ftn_supported,omitempty"`
	MSISDNLessOperationSupported bool  `json:"msisdn_less_operation_supported,omitempty"`
	MTRoamingForwardingSupported bool  `json:"mt_roaming_forwarding_supported,omitempty"`
	SOLSASupported               bool  `json:"solsa_supported,omitempty"`
	LongGroupIDSupported         bool  `json:"long_group_id_supported,omitempty"`
	ResetIDsSupported            bool  `json:"reset_ids_supported,omitempty"`
	// ISTSupport is basicISTSupported or istCommandSupported.
	ISTSupport string `json:"ist_support,omitempty"`
	// SuperCharger is the VLR's Super-Charger information (TS 23.116).
	SuperCharger *SuperCharger `json:"super_charger,omitempty"`
}

// SuperCharger is TS 29.002 SuperChargerInfo: the VLR asks for the data,
// or holds data of the given age (AgeIndicator, hex).
type SuperCharger struct {
	SendSubscriberData bool   `json:"send_subscriber_data,omitempty"`
	SubscriberDataAge  string `json:"subscriber_data_age,omitempty"`
}

// SubscriberData is the circuit-switched subscriber data the gateway sends
// a VLR in insertSubscriberData (TS 29.002 SubscriberData, §8.8.1.3 for
// what each field means and when to send it). Codes are hex, one octet
// each unless the type allows more; names are the ASN.1 value names.
// CUG, eMLPP, VBS/VGCS and CAMEL subscription information are not carried.
type SubscriberData struct {
	MSISDN *AddressString `json:"msisdn,omitempty"`
	// Category is the ITU-T Q.763 calling party's category, hex (0a:
	// ordinary subscriber).
	Category string `json:"category,omitempty"`
	// SubscriberStatus is serviceGranted or operatorDeterminedBarring (then
	// ODB is required).
	SubscriberStatus string `json:"subscriber_status,omitempty"`
	// BearerServices and Teleservices are Ext-BearerServiceCode and
	// Ext-TeleserviceCode values, hex (11: telephony, 21:
	// shortMessageMT-PP, 22: shortMessageMO-PP).
	BearerServices []string          `json:"bearer_services,omitempty"`
	Teleservices   []string          `json:"teleservices,omitempty"`
	Forwarding     []ForwardingInfo  `json:"forwarding,omitempty"`
	CallBarring    []CallBarringInfo `json:"call_barring,omitempty"`
	SSData         []SSData          `json:"ss_data,omitempty"`
	ODB            *ODB              `json:"odb,omitempty"`
	// RoamingRestrictedUnsupportedFeature is
	// roamingRestrictionDueToUnsupportedFeature.
	RoamingRestrictedUnsupportedFeature bool `json:"roaming_restricted_unsupported_feature,omitempty"`
	// RegionalSubscription are zone codes (TS 23.003 §4.4), hex, 2 octets
	// each; at most 10.
	RegionalSubscription []string `json:"regional_subscription,omitempty"`
	// NetworkAccessMode is packetAndCircuit, onlyCircuit or onlyPacket.
	NetworkAccessMode string `json:"network_access_mode,omitempty"`
	// AccessRestriction are AccessRestrictionData names: utranNotAllowed,
	// geranNotAllowed, ganNotAllowed, i-hspa-evolutionNotAllowed,
	// wb-e-utranNotAllowed, ho-toNon3GPP-AccessNotAllowed,
	// nb-iotNotAllowed, enhancedCoverageNotAllowed.
	AccessRestriction []string `json:"access_restriction,omitempty"`
}

// SSStatus is TS 29.002 Ext-SS-Status: the TS 23.011 state bits.
type SSStatus struct {
	Provisioned bool `json:"provisioned,omitempty"` // P
	Registered  bool `json:"registered,omitempty"`  // R
	Active      bool `json:"active,omitempty"`      // A
	Quiescent   bool `json:"quiescent,omitempty"`   // Q
}

// ForwardingInfo is TS 29.002 Ext-ForwInfo: one call forwarding service
// (SSCode, hex: 21 cfu, 29 cfb, 2a cfnry, 2b cfnrc) and its features.
type ForwardingInfo struct {
	SSCode   string              `json:"ss_code"`
	Features []ForwardingFeature `json:"features"`
}

// ForwardingFeature is TS 29.002 Ext-ForwFeature.
type ForwardingFeature struct {
	BasicService *BasicService `json:"basic_service,omitempty"`
	Status       SSStatus      `json:"status"`
	// ForwardedToNumber goes as longForwardedToNumber when it is longer
	// than an ISDN-AddressString holds; send that only to a VLR whose
	// capability says LongFTNSupported.
	ForwardedToNumber     *AddressString     `json:"forwarded_to_number,omitempty"`
	ForwardedToSubaddress string             `json:"forwarded_to_subaddress,omitempty"` // hex
	Options               *ForwardingOptions `json:"options,omitempty"`
	// NoReplyTime is the no reply condition timer of cfnry, 5 to 30 s.
	NoReplyTime int `json:"no_reply_time,omitempty"`
}

// CallBarringInfo is TS 29.002 Ext-CallBarInfo (SSCode, hex: 92 baoc,
// 93 boic, 94 boic-exHC, 9a baic, 9b bic-roam).
type CallBarringInfo struct {
	SSCode   string               `json:"ss_code"`
	Features []CallBarringFeature `json:"features"`
}

// CallBarringFeature is TS 29.002 Ext-CallBarringFeature.
type CallBarringFeature struct {
	BasicService *BasicService `json:"basic_service,omitempty"`
	Status       SSStatus      `json:"status"`
}

// SSData is TS 29.002 Ext-SS-Data: a supplementary service other than call
// forwarding and barring (SSCode, hex: 11 clip, 12 clir, 41 cw, 42 hold,
// 51 multiPTY).
type SSData struct {
	SSCode string   `json:"ss_code"`
	Status SSStatus `json:"status"`
	// CLIRestriction is clir's option: permanent, temporaryDefaultRestricted
	// or temporaryDefaultAllowed. OverrideCategory (overrideEnabled or
	// overrideDisabled) is the alternative: at most one of them.
	CLIRestriction   string         `json:"cli_restriction,omitempty"`
	OverrideCategory string         `json:"override_category,omitempty"`
	BasicServices    []BasicService `json:"basic_services,omitempty"`
}

// ODB is TS 29.002 ODB-Data: the operator determined barring categories
// that apply, by ASN.1 name. General is ODB-GeneralData (allOG-CallsBarred,
// internationalOGCallsBarred, …); HPLMN is ODB-HPLMN-Data
// (plmn-SpecificBarringType1 … 4).
type ODB struct {
	General []string `json:"general"`
	HPLMN   []string `json:"hplmn,omitempty"`
}

// InsertResult is what a VLR answered to subscriber data
// (InsertSubscriberDataRes): what it did not take. Data sent in several
// parts gives one result, the parts' together.
type InsertResult struct {
	UnsupportedTeleservices   []string `json:"unsupported_teleservices,omitempty"`
	UnsupportedBearerServices []string `json:"unsupported_bearer_services,omitempty"`
	UnsupportedSSCodes        []string `json:"unsupported_ss_codes,omitempty"`
	// UnsupportedODB are the ODB-GeneralData categories it does not support.
	UnsupportedODB []string `json:"unsupported_odb,omitempty"`
	// RegionalSubscriptionResponse: networkNode-AreaRestricted,
	// tooManyZoneCodes, zoneCodesConflict or regionalSubscNotSupported.
	RegionalSubscriptionResponse string   `json:"regional_subscription_response,omitempty"`
	SupportedCAMELPhases         []int    `json:"supported_camel_phases,omitempty"`
	OfferedCAMEL4CSIs            []string `json:"offered_camel4_csis,omitempty"`
}

// RoamingNumberEnquiry is ProvideRoamingNumber's part of a Message: the
// request's fields, and the answer's.
type RoamingNumberEnquiry struct {
	// Request (ProvideRoamingNumberArg). The IMSI is in Message.IMSI.
	MSCNumber                 *AddressString `json:"msc_number,omitempty"`
	MSISDN                    *AddressString `json:"msisdn,omitempty"`
	LMSI                      string         `json:"lmsi,omitempty"` // hex, 4 octets
	GMSCAddress               *AddressString `json:"gmsc_address,omitempty"`
	CallReferenceNumber       string         `json:"call_reference_number,omitempty"` // hex, 1..8 octets
	ORInterrogation           bool           `json:"or_interrogation,omitempty"`
	SuppressionOfAnnouncement bool           `json:"suppression_of_announcement,omitempty"`
	// BearerCapability is gsm-BearerCapability: the ExternalSignalInfo
	// protocol (1 gsm-0408, 2 gsm-0806, 3 gsm-BSSMAP, 4 ets-300102-1) and
	// signal octets, hex.
	BearerCapability *SignalInfo `json:"bearer_capability,omitempty"`

	// Answer (ProvideRoamingNumberRes).
	RoamingNumber             *AddressString `json:"roaming_number,omitempty"`
	VMSCAddress               *AddressString `json:"vmsc_address,omitempty"`
	ReleaseResourcesSupported bool           `json:"release_resources_supported,omitempty"`
}

// SignalInfo is TS 29.002 ExternalSignalInfo.
type SignalInfo struct {
	ProtocolID int    `json:"protocol_id"`
	Data       string `json:"data"` // hex, 1..200 octets
}

// CancellationType values of CancelLocation.
const (
	CancellationUpdateProcedure      = "updateProcedure"
	CancellationSubscriptionWithdraw = "subscriptionWithdraw"
)

// CancelLocation is CancelLocation's part of a request Message.
type CancelLocation struct {
	LMSI string `json:"lmsi,omitempty"` // hex, 4 octets: the identity becomes imsi-WithLMSI
	// Type is the cancellationType, CancellationUpdateProcedure or
	// CancellationSubscriptionWithdraw; empty leaves it out.
	Type string `json:"type,omitempty"`
}

// ----- location updates ---------------------------------------------------

// LocationUpdateTurn is a turn of a location update as the handler gets it.
type LocationUpdateTurn struct {
	DialogueID string
	Action     string // ULUpdateLocation, ULInsertResult or ULEnd
	IMSI       string
	// Update is the VLR's request: on every turn.
	Update *LocationUpdate
	// Insert is the VLR's answer to the subscriber data (ULInsertResult
	// with an empty Status).
	Insert *InsertResult
	// Status, Code and Detail: a failure — of the subscriber data
	// (ULInsertResult: StatusRemoteError and the VLR's MAP error, or
	// StatusRejected when the gateway could not send it), or how the
	// location update ended (ULEnd: StatusAborted, StatusTimeout).
	Status string
	Code   int32
	Detail string
	NodeID string
	Raw    *Message
}

// LocationUpdateAnswer is the application's answer to a turn: exactly one
// of Insert, Accept and Reject.
type LocationUpdateAnswer struct {
	// Insert sends the subscriber data (insertSubscriberData); the next turn
	// is its result. The gateway divides data that does not fit one message
	// into several, and gives one result.
	Insert *SubscriberData
	// Accept ends the location update with updateLocation's result, the
	// gateway's HLR number in it.
	Accept bool
	// Reject ends it with updateLocation's error: MAPErrSystemFailure,
	// MAPErrDataMissing, MAPErrUnexpectedDataValue, MAPErrUnknownSubscriber
	// or MAPErrRoamingNotAllowed.
	Reject int32
}

// ServeLocationUpdates makes the application the HLR's location management
// for location updates (TS 29.002 §19.1.1.5).
//
// A VLR's updateLocation is a turn ULUpdateLocation. The handler answers
// it: Insert subscriber data, Accept, or Reject. After an insert the next
// turn is ULInsertResult — what the VLR did not take, or the failure — and
// the handler answers again: more data, Accept or Reject.
//
// A location update ends in one of two ways. Either the gateway carries
// out the application's Accept or Reject — updateLocation's result or
// error goes to the VLR — and that is all; or it gets exactly one ULEnd
// turn, whose answer is not used. A ULEnd comes when:
//   - the VLR or the network ends it (an abort, the VLR's silence, a
//     timeout, the gateway shutting down): StatusAborted, StatusTimeout;
//   - the gateway refuses it with systemFailure on its own account — the
//     answer did not come by AnswerBy (StatusTimeout), did not parse, or
//     is not one it can carry out (StatusRejected);
//   - the result or error could not be sent (StatusAborted).
//
// A ULEnd always comes after the turn being answered, if any: a ULEnd
// that follows an answer means that answer was not carried out (undo what
// it committed, such as a registration).
//
// Each turn runs handle on its own goroutine with a context ending at
// AnswerBy; turns of one location update come one at a time. With queue
// non-empty the subscription is in that queue group, so instances of the
// application share the location updates — a later turn may reach another
// instance, so keep the state of a location update in what DialogueID
// names, not in memory.
func (c *Client) ServeLocationUpdates(queue string, handle func(ctx context.Context, t *LocationUpdateTurn) LocationUpdateAnswer) (*nats.Subscription, error) {
	cb := func(m *nats.Msg) {
		var msg Message
		if err := json.Unmarshal(m.Data, &msg); err != nil || msg.LocationUpdate == nil {
			if c.onDecodeError != nil {
				if err == nil {
					err = errors.New("no location_update")
				}
				c.onDecodeError(m.Subject, m.Data, err)
			}
			return
		}
		go c.serveTurn(m, &msg, handle)
	}
	if queue == "" {
		return c.nc.Subscribe(SubjectLocationUpdate, cb)
	}
	return c.nc.QueueSubscribe(SubjectLocationUpdate, queue, cb)
}

func (c *Client) serveTurn(m *nats.Msg, msg *Message, handle func(context.Context, *LocationUpdateTurn) LocationUpdateAnswer) {
	u := msg.LocationUpdate
	ctx := context.Background()
	if u.AnswerBy != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, *u.AnswerBy)
		defer cancel()
	}
	turn := &LocationUpdateTurn{
		DialogueID: u.DialogueID, Action: u.Action, IMSI: msg.IMSI, Update: u, Insert: msg.InsertResult,
		Status: msg.Status, Code: msg.ErrorCode, Detail: u.Detail, NodeID: msg.NodeID, Raw: msg,
	}
	if turn.Detail == "" {
		turn.Detail = msg.ErrorMessage
	}
	answer := handle(ctx, turn)
	if u.Action == ULEnd || m.Reply == "" {
		return
	}
	reply := &Message{
		MessageID: uuid.NewString(), CorrelationID: msg.CorrelationID, MessageType: MsgTypeMAPUpdateLocation,
		Direction: DirectionInbound, Timestamp: time.Now().UTC(),
		LocationUpdate: &LocationUpdate{DialogueID: u.DialogueID},
	}
	switch {
	case answer.Insert != nil && !answer.Accept && answer.Reject == 0:
		if err := answer.Insert.validate("location update insert"); err != nil {
			reply.LocationUpdate.Action, reply.LocationUpdate.Error = ULReject, MAPErrSystemFailure
			reply.LocationUpdate.Detail = err.Error()
			break
		}
		reply.LocationUpdate.Action, reply.SubscriberData = ULInsert, answer.Insert
	case answer.Accept && answer.Insert == nil && answer.Reject == 0:
		reply.LocationUpdate.Action = ULAccept
	case answer.Reject != 0 && answer.Insert == nil && !answer.Accept && updateLocationRejectCodes[answer.Reject]:
		reply.LocationUpdate.Action, reply.LocationUpdate.Error = ULReject, answer.Reject
	default:
		reply.LocationUpdate.Action, reply.LocationUpdate.Error = ULReject, MAPErrSystemFailure
		reply.LocationUpdate.Detail = "the application's answer was not one Insert, Accept or Reject with an updateLocation error"
	}
	data, err := json.Marshal(reply)
	if err == nil {
		err = m.Respond(data)
	}
	if err != nil && c.onDecodeError != nil {
		c.onDecodeError(m.Subject, nil, fmt.Errorf("location update answer not sent: %w", err))
	}
}

// ----- requests to a VLR ----------------------------------------------------

// CancelLocationRequest is a cancelLocation to a VLR (TS 29.002 §8.1.3).
type CancelLocationRequest struct {
	IMSI      string // required
	VLRNumber string // required: the VLR's number, E.164 digits
	// VLRPointCode routes on DPC/SSN instead of the VLR number as GT.
	VLRPointCode uint32
	LMSI         string // hex, 4 octets
	Type         string // CancellationUpdateProcedure, CancellationSubscriptionWithdraw or ""
}

// CancelLocation tells a VLR to delete a subscriber, under
// locationCancellationContext-v3. A nil error is the VLR's result.
func (c *Client) CancelLocation(ctx context.Context, req CancelLocationRequest) error {
	if err := checkVLRRequest("CancelLocation", req.IMSI, req.VLRNumber); err != nil {
		return err
	}
	if err := checkHex("CancelLocation", "LMSI", req.LMSI, 4, 4); err != nil {
		return err
	}
	if req.Type != "" && req.Type != CancellationUpdateProcedure && req.Type != CancellationSubscriptionWithdraw {
		return fmt.Errorf("%w: CancelLocation: Type %q", ErrInvalidRequest, req.Type)
	}
	_, err := c.request(ctx, SubjectCancelLocationRequest, &Message{
		MessageType: MsgTypeMAPCancelLocation, IMSI: req.IMSI, DPC: req.VLRPointCode,
		Parameters:     map[string]interface{}{ParamVLRNumber: req.VLRNumber},
		CancelLocation: &CancelLocation{LMSI: req.LMSI, Type: req.Type},
	})
	return err
}

// PRNRequest is a provideRoamingNumber to the VLR serving a subscriber
// (TS 29.002 §10.2).
type PRNRequest struct {
	IMSI      string // required
	VLRNumber string // required: E.164 digits
	// VLRPointCode routes on DPC/SSN instead of the VLR number as GT.
	VLRPointCode uint32
	// Enquiry carries the argument; MSCNumber is required (the MSC number
	// the location update gave).
	Enquiry RoamingNumberEnquiry
}

// PRNResult is a roaming number.
type PRNResult struct {
	RoamingNumber             AddressString
	VMSCAddress               *AddressString
	ReleaseResourcesSupported bool
	NodeID                    string
	Raw                       *Message
}

// ProvideRoamingNumber asks a VLR for a roaming number (MSRN), under
// roamingNumberEnquiryContext-v3.
func (c *Client) ProvideRoamingNumber(ctx context.Context, req PRNRequest) (*PRNResult, error) {
	if err := checkVLRRequest("ProvideRoamingNumber", req.IMSI, req.VLRNumber); err != nil {
		return nil, err
	}
	e := req.Enquiry
	if e.RoamingNumber != nil || e.VMSCAddress != nil || e.ReleaseResourcesSupported {
		return nil, fmt.Errorf("%w: ProvideRoamingNumber: answer fields in the request", ErrInvalidRequest)
	}
	if e.MSCNumber == nil {
		return nil, fmt.Errorf("%w: ProvideRoamingNumber: Enquiry.MSCNumber required", ErrInvalidRequest)
	}
	for name, a := range map[string]*AddressString{"MSCNumber": e.MSCNumber, "MSISDN": e.MSISDN, "GMSCAddress": e.GMSCAddress} {
		if err := checkAddress("ProvideRoamingNumber", name, a); err != nil {
			return nil, err
		}
	}
	if err := checkHex("ProvideRoamingNumber", "LMSI", e.LMSI, 4, 4); err != nil {
		return nil, err
	}
	if err := checkHex("ProvideRoamingNumber", "CallReferenceNumber", e.CallReferenceNumber, 1, 8); err != nil {
		return nil, err
	}
	if b := e.BearerCapability; b != nil {
		if b.ProtocolID < 1 || b.ProtocolID > 4 {
			return nil, fmt.Errorf("%w: ProvideRoamingNumber: BearerCapability.ProtocolID %d", ErrInvalidRequest, b.ProtocolID)
		}
		if err := checkHex("ProvideRoamingNumber", "BearerCapability.Data", b.Data, 1, 200); err != nil {
			return nil, err
		}
		if b.Data == "" {
			return nil, fmt.Errorf("%w: ProvideRoamingNumber: BearerCapability.Data required", ErrInvalidRequest)
		}
	}
	resp, err := c.request(ctx, SubjectPRNRequest, &Message{
		MessageType: MsgTypeMAPProvideRoamingNumber, IMSI: req.IMSI, DPC: req.VLRPointCode,
		Parameters:           map[string]interface{}{ParamVLRNumber: req.VLRNumber},
		RoamingNumberEnquiry: &e,
	})
	if err != nil {
		return nil, err
	}
	if resp.RoamingNumberEnquiry == nil || resp.RoamingNumberEnquiry.RoamingNumber == nil {
		return nil, errors.New("ss7 client: successful response without roaming_number_enquiry.roaming_number")
	}
	a := resp.RoamingNumberEnquiry
	return &PRNResult{RoamingNumber: *a.RoamingNumber, VMSCAddress: a.VMSCAddress,
		ReleaseResourcesSupported: a.ReleaseResourcesSupported, NodeID: resp.NodeID, Raw: resp}, nil
}

// ISDRequest is a stand-alone insertSubscriberData to the VLR serving a
// subscriber (TS 29.002 §8.8.1): changed or added data.
type ISDRequest struct {
	IMSI      string // required
	VLRNumber string // required: E.164 digits
	// VLRPointCode routes on DPC/SSN instead of the VLR number as GT.
	VLRPointCode uint32
	Data         SubscriberData
}

// ISDResult is the VLR's answer.
type ISDResult struct {
	Result *InsertResult // never nil
	NodeID string
	Raw    *Message
}

// InsertSubscriberData sends subscriber data to a VLR, under
// subscriberDataMngtContext-v3. The data must fit one message: the gateway
// refuses (StatusRejected) what does not, and it can be sent in several
// requests.
func (c *Client) InsertSubscriberData(ctx context.Context, req ISDRequest) (*ISDResult, error) {
	if err := checkVLRRequest("InsertSubscriberData", req.IMSI, req.VLRNumber); err != nil {
		return nil, err
	}
	if err := req.Data.validate("InsertSubscriberData"); err != nil {
		return nil, err
	}
	data := req.Data
	resp, err := c.request(ctx, SubjectISDRequest, &Message{
		MessageType: MsgTypeMAPInsertSubscriberData, IMSI: req.IMSI, DPC: req.VLRPointCode,
		Parameters:     map[string]interface{}{ParamVLRNumber: req.VLRNumber},
		SubscriberData: &data,
	})
	if err != nil {
		return nil, err
	}
	res := resp.InsertResult
	if res == nil {
		res = &InsertResult{}
	}
	return &ISDResult{Result: res, NodeID: resp.NodeID, Raw: resp}, nil
}

// ----- validation -------------------------------------------------------------

func checkVLRRequest(op, imsi, vlr string) error {
	if imsi == "" || vlr == "" {
		return fmt.Errorf("%w: %s: IMSI and VLRNumber required", ErrInvalidRequest, op)
	}
	if err := checkDigits(op, map[string]string{"IMSI": imsi, "VLRNumber": vlr}); err != nil {
		return err
	}
	if len(imsi) < 5 || len(imsi) > 15 {
		return fmt.Errorf("%w: %s: IMSI of %d digits", ErrInvalidRequest, op, len(imsi))
	}
	return checkISDNLength(op, "VLRNumber", vlr)
}

// checkHex: s empty, or hex of min..max octets.
func checkHex(op, name, s string, min, max int) error {
	if s == "" {
		return nil
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) < min || len(b) > max {
		return fmt.Errorf("%w: %s: %s is not hex of %d..%d octets", ErrInvalidRequest, op, name, min, max)
	}
	return nil
}

func checkAddress(op, name string, a *AddressString) error {
	if a == nil {
		return nil
	}
	if err := checkDigits(op, map[string]string{name: a.Digits}); err != nil {
		return err
	}
	if a.Digits == "" {
		return fmt.Errorf("%w: %s: %s without digits", ErrInvalidRequest, op, name)
	}
	return checkISDNLength(op, name, a.Digits)
}

// Names the gateway accepts in SubscriberData (TS 29.002 ASN.1 value names),
// the named bits in bit order and the enumerations in value order.
var (
	odbGeneralNames = []string{
		"allOG-CallsBarred", "internationalOGCallsBarred", "internationalOGCallsNotToHPLMN-CountryBarred",
		"premiumRateInformationOGCallsBarred", "premiumRateEntertainementOGCallsBarred", "ss-AccessBarred",
		"interzonalOGCallsBarred", "interzonalOGCallsNotToHPLMN-CountryBarred",
		"interzonalOGCallsAndInternationalOGCallsNotToHPLMN-CountryBarred", "allECT-Barred",
		"chargeableECT-Barred", "internationalECT-Barred", "interzonalECT-Barred", "doublyChargeableECT-Barred",
		"multipleECT-Barred", "allPacketOrientedServicesBarred", "roamerAccessToHPLMN-AP-Barred",
		"roamerAccessToVPLMN-AP-Barred", "roamingOutsidePLMNOG-CallsBarred", "allIC-CallsBarred",
		"roamingOutsidePLMNIC-CallsBarred", "roamingOutsidePLMNICountryIC-CallsBarred", "roamingOutsidePLMN-Barred",
		"roamingOutsidePLMN-CountryBarred", "registrationAllCF-Barred", "registrationCFNotToHPLMN-Barred",
		"registrationInterzonalCF-Barred", "registrationInterzonalCFNotToHPLMN-Barred",
		"registrationInternationalCF-Barred",
	}
	odbHPLMNNames          = []string{"plmn-SpecificBarringType1", "plmn-SpecificBarringType2", "plmn-SpecificBarringType3", "plmn-SpecificBarringType4"}
	accessRestrictionNames = []string{"utranNotAllowed", "geranNotAllowed", "ganNotAllowed", "i-hspa-evolutionNotAllowed",
		"wb-e-utranNotAllowed", "ho-toNon3GPP-AccessNotAllowed", "nb-iotNotAllowed", "enhancedCoverageNotAllowed"}
	cliRestrictionNames = []string{"permanent", "temporaryDefaultRestricted", "temporaryDefaultAllowed"}
	overrideNames       = []string{"overrideEnabled", "overrideDisabled"}
	nonEmptyNames       = map[string][]string{
		"SubscriberStatus":  {"serviceGranted", "operatorDeterminedBarring"},
		"NetworkAccessMode": {"packetAndCircuit", "onlyCircuit", "onlyPacket"},
	}
)

// validate checks what the gateway will check, so that a mistake is an
// error here rather than a refusal on the network path.
func (d *SubscriberData) validate(op string) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s: subscriber data: %s", ErrInvalidRequest, op, fmt.Sprintf(format, args...))
	}
	if err := checkAddress(op, "MSISDN", d.MSISDN); err != nil {
		return err
	}
	if d.Category != "" {
		if err := checkHex(op, "Category", d.Category, 1, 1); err != nil {
			return err
		}
	}
	for field, value := range map[string]string{"SubscriberStatus": d.SubscriberStatus, "NetworkAccessMode": d.NetworkAccessMode} {
		if value != "" && !slices.Contains(nonEmptyNames[field], value) {
			return bad("%s %q", field, value)
		}
	}
	if (d.SubscriberStatus == "operatorDeterminedBarring") != (d.ODB != nil) {
		return bad("ODB goes with SubscriberStatus operatorDeterminedBarring, and only with it (TS 29.002 §8.8.1.3)")
	}
	for _, l := range []struct {
		name  string
		codes []string
		max   int
	}{{"BearerServices", d.BearerServices, 50}, {"Teleservices", d.Teleservices, 20}} {
		if len(l.codes) > l.max {
			return bad("%d %s, at most %d", len(l.codes), l.name, l.max)
		}
		for _, c := range l.codes {
			if c == "" || checkHex(op, l.name, c, 1, 5) != nil {
				return bad("%s code %q is not hex of 1..5 octets", l.name, c)
			}
		}
	}
	if n := len(d.Forwarding) + len(d.CallBarring) + len(d.SSData); n > 30 {
		return bad("%d supplementary services, at most 30", n)
	}
	ssCode := func(c string) error {
		if c == "" || checkHex(op, "SSCode", c, 1, 1) != nil {
			return bad("SS code %q is not one hex octet", c)
		}
		return nil
	}
	basic := func(b *BasicService) error {
		if b == nil {
			return nil
		}
		if b.Kind != "ext-BearerService" && b.Kind != "ext-Teleservice" {
			return bad("basic service kind %q", b.Kind)
		}
		if b.Code == "" || checkHex(op, "BasicService.Code", b.Code, 1, 5) != nil {
			return bad("basic service code %q", b.Code)
		}
		return nil
	}
	for _, f := range d.Forwarding {
		if err := ssCode(f.SSCode); err != nil {
			return err
		}
		if len(f.Features) < 1 || len(f.Features) > 32 {
			return bad("forwarding %s: %d features, want 1..32", f.SSCode, len(f.Features))
		}
		for _, ft := range f.Features {
			if err := basic(ft.BasicService); err != nil {
				return err
			}
			if err := checkHex(op, "ForwardedToSubaddress", ft.ForwardedToSubaddress, 1, 21); err != nil {
				return err
			}
			if ft.NoReplyTime != 0 && (ft.NoReplyTime < 5 || ft.NoReplyTime > 30) {
				return bad("NoReplyTime %d, want 5..30", ft.NoReplyTime)
			}
			if a := ft.ForwardedToNumber; a != nil && (a.Digits == "" || len(a.Digits) > 28) {
				return bad("forwarded-to number of %d digits", len(a.Digits))
			}
			if o := ft.Options; o != nil && !slices.Contains([]string{"notReachable", "busy", "noReply", "unconditional"}, o.Reason) {
				return bad("forwarding options reason %q", o.Reason)
			}
		}
	}
	for _, b := range d.CallBarring {
		if err := ssCode(b.SSCode); err != nil {
			return err
		}
		if len(b.Features) < 1 || len(b.Features) > 32 {
			return bad("call barring %s: %d features, want 1..32", b.SSCode, len(b.Features))
		}
		for _, ft := range b.Features {
			if err := basic(ft.BasicService); err != nil {
				return err
			}
		}
	}
	for _, s := range d.SSData {
		if err := ssCode(s.SSCode); err != nil {
			return err
		}
		if s.CLIRestriction != "" && s.OverrideCategory != "" {
			return bad("SS data %s: CLIRestriction and OverrideCategory are alternatives", s.SSCode)
		}
		if s.CLIRestriction != "" && !slices.Contains(cliRestrictionNames, s.CLIRestriction) {
			return bad("CLIRestriction %q", s.CLIRestriction)
		}
		if s.OverrideCategory != "" && !slices.Contains(overrideNames, s.OverrideCategory) {
			return bad("OverrideCategory %q", s.OverrideCategory)
		}
		if len(s.BasicServices) > 32 {
			return bad("SS data %s: %d basic services, at most 32", s.SSCode, len(s.BasicServices))
		}
		for i := range s.BasicServices {
			if err := basic(&s.BasicServices[i]); err != nil {
				return err
			}
		}
	}
	if o := d.ODB; o != nil {
		for _, n := range o.General {
			if !slices.Contains(odbGeneralNames, n) {
				return bad("ODB general category %q", n)
			}
		}
		for _, n := range o.HPLMN {
			if !slices.Contains(odbHPLMNNames, n) {
				return bad("ODB HPLMN category %q", n)
			}
		}
	}
	if len(d.RegionalSubscription) > 10 {
		return bad("%d zone codes, at most 10", len(d.RegionalSubscription))
	}
	for _, z := range d.RegionalSubscription {
		if z == "" || checkHex(op, "RegionalSubscription", z, 2, 2) != nil {
			return bad("zone code %q is not 2 hex octets", z)
		}
	}
	for _, n := range d.AccessRestriction {
		if !slices.Contains(accessRestrictionNames, n) {
			return bad("access restriction %q", n)
		}
	}
	return nil
}

// The named-bit names of the BIT STRINGs above, in bit order: a name's
// index is its bit (TS 29.002 ASN.1). The gateway maps by them.
func ODBGeneralNames() []string        { return slices.Clone(odbGeneralNames) }
func ODBHPLMNNames() []string          { return slices.Clone(odbHPLMNNames) }
func AccessRestrictionNames() []string { return slices.Clone(accessRestrictionNames) }
func RATTypeNames() []string           { return slices.Clone(ratTypeNames) }
func CAMEL4CSINames() []string         { return slices.Clone(camel4CSINames) }

// The ENUMERATED names, by value.
func CLIRestrictionNames() []string   { return slices.Clone(cliRestrictionNames) }
func OverrideCategoryNames() []string { return slices.Clone(overrideNames) }
func RegionalSubscriptionResponseNames() []string {
	return slices.Clone(regionalSubscriptionResponseNames)
}
func SubscriberStatusNames() []string  { return slices.Clone(nonEmptyNames["SubscriberStatus"]) }
func NetworkAccessModeNames() []string { return slices.Clone(nonEmptyNames["NetworkAccessMode"]) }

var (
	ratTypeNames                      = []string{"utran", "geran", "gan", "i-hspa-evolution", "e-utran", "nb-iot"}
	camel4CSINames                    = []string{"o-csi", "d-csi", "vt-csi", "t-csi", "mt-sms-csi", "mg-csi", "psi-enhancements"}
	regionalSubscriptionResponseNames = []string{"networkNode-AreaRestricted", "tooManyZoneCodes", "zoneCodesConflict", "regionalSubscNotSupported"}
)
