// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: Apache-2.0

package ss7client

import (
	"fmt"
	"time"
)

// NATS subjects of the MAP SMS interface. The gateway queue-subscribes to the
// request subjects (queue group = its cluster name) and publishes on the
// response subjects.
const (
	// SubjectSRIRequest carries SRI-for-SM requests, application → gateway.
	SubjectSRIRequest = "map.sri.request"
	// SubjectSRIResponse carries SRI-for-SM results and failures, gateway → application.
	SubjectSRIResponse = "map.sri.response"
	// SubjectMTSMSRequest carries MT-Forward-SM requests, application → gateway.
	SubjectMTSMSRequest = "map.mt.sms.request"
	// SubjectMTSMSResponse carries MT-Forward-SM results and failures, gateway → application.
	SubjectMTSMSResponse = "map.mt.sms.response"
	// SubjectMOSMS carries MO-Forward-SM received from a foreign MSC, gateway → application.
	SubjectMOSMS = "map.mo.sms"
)

// MessageType identifies the operation a [Message] belongs to.
type MessageType string

const (
	MsgTypeMAPSRI   MessageType = "MAP_SRI"
	MsgTypeMAPMTSMS MessageType = "MAP_MT_SMS"
	MsgTypeMAPMOSMS MessageType = "MAP_MO_SMS"
)

// Direction is relative to the SS7 network: Inbound goes into it (application
// → gateway), Outbound comes out of it (gateway → application).
type Direction string

const (
	DirectionInbound  Direction = "INBOUND"
	DirectionOutbound Direction = "OUTBOUND"
)

// Message is the JSON document carried on every subject. Field names and JSON
// tags are the wire contract shared with non-Go consumers; changing one breaks
// them.
type Message struct {
	MessageID     string      `json:"message_id"`
	CorrelationID string      `json:"correlation_id,omitempty"`
	MessageType   MessageType `json:"message_type"`
	Direction     Direction   `json:"direction"`
	Timestamp     time.Time   `json:"timestamp"`

	// Addressing (E.164 digits).
	Sender    string `json:"sender,omitempty"`
	Recipient string `json:"recipient,omitempty"`
	IMSI      string `json:"imsi,omitempty"`
	MSISDN    string `json:"msisdn,omitempty"`

	// SS7 routing overrides. Zero/empty means "use the gateway's configuration".
	OPC         uint32 `json:"opc,omitempty"`
	DPC         uint32 `json:"dpc,omitempty"`
	SSN         uint8  `json:"ssn,omitempty"`
	GlobalTitle string `json:"global_title,omitempty"`

	Content     string                 `json:"content,omitempty"`
	PayloadData []byte                 `json:"payload_data,omitempty"` // base64 in JSON
	Parameters  map[string]interface{} `json:"parameters,omitempty"`

	ServiceKey uint32 `json:"service_key,omitempty"`
	EventType  string `json:"event_type,omitempty"`

	// Location interface (map.ati.*, map.psi.*): what a request asks for
	// and what the answer carries. SubscriberInfo is also in a call routing
	// answer when the HLR returns the subscriber's location or state.
	RequestedInfo  *RequestedInfo  `json:"requested_info,omitempty"`
	SubscriberInfo *SubscriberInfo `json:"subscriber_info,omitempty"`

	// Call routing (map.routing.*): the answer's routing information. The
	// answer's IMSI is in IMSI, its location and state, if any, in
	// SubscriberInfo.
	RoutingInfo *RoutingInfo `json:"routing_info,omitempty"`

	// USSD (map.ussd.*): the session, the action, and the text.
	USSD *USSD `json:"ussd,omitempty"`

	// Location management with the gateway as HLR: a location update's
	// turn or answer (map.ul); the subscriber data of an insert (map.ul
	// answer, map.isd.request) and the VLR's answer to it (map.ul turn,
	// map.isd.response); cancelLocation's and provideRoamingNumber's fields
	// (map.cl.*, map.prn.*).
	LocationUpdate       *LocationUpdate       `json:"location_update,omitempty"`
	SubscriberData       *SubscriberData       `json:"subscriber_data,omitempty"`
	InsertResult         *InsertResult         `json:"insert_result,omitempty"`
	CancelLocation       *CancelLocation       `json:"cancel_location,omitempty"`
	RoamingNumberEnquiry *RoamingNumberEnquiry `json:"roaming_number_enquiry,omitempty"`

	// Authentication and MS purging with the gateway as HLR: a
	// sendAuthenticationInfo or purgeMS and the application's answer
	// (map.sai, map.purge).
	Authentication *Authentication `json:"authentication,omitempty"`
	Purge          *Purge          `json:"purge,omitempty"`

	Status       string `json:"status,omitempty"`
	ErrorCode    int32  `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`

	TraceID string `json:"trace_id,omitempty"`
	NodeID  string `json:"node_id,omitempty"` // gateway node that handled the dialogue
}

// Keys of Message.Parameters understood or produced by the gateway.
const (
	// ParamServiceCentreAddress (request, SRI and MT): the SMSC address, as
	// a digit string or an [AddressString] object.
	ParamServiceCentreAddress = "service_centre_address"
	// ParamNetworkNodeNumber (request, MT): the serving MSC/VLR address
	// returned by SRI-for-SM, as a digit string or an [AddressString] object.
	ParamNetworkNodeNumber = "network_node_number"
	// ParamTPDUBase64 (request, MT): the TPDU as standard base64, an
	// alternative to Message.PayloadData.
	ParamTPDUBase64 = "tpdu_base64"
	// ParamVMSCAddress (response, SRI): digits of the network node number
	// returned by the HLR.
	ParamVMSCAddress = "vmsc_address"
)

// Values of Message.Status on response subjects.
const (
	// StatusDelivered: MT-Forward-SM ended with a ReturnResult. A successful
	// SRI-for-SM, AnyTimeInterrogation, ProvideSubscriberInfo or
	// SendRoutingInfo response carries an empty status.
	StatusDelivered = "DELIVERED"
	// StatusTimeout: the gateway's TCAP dialogue timer expired.
	StatusTimeout = "TIMEOUT"
	// StatusAborted: the peer aborted the dialogue (TC-P-ABORT / TC-U-ABORT).
	StatusAborted = "ABORTED"
	// StatusRejected: the gateway refused the request before starting a
	// dialogue — a missing or invalid field, or no capacity for a new
	// dialogue. ErrorCode is ErrCodeRejected and ErrorMessage the reason.
	StatusRejected = "REJECTED"
	// StatusRemoteError: the peer answered with a MAP ReturnError — ErrorCode
	// is the MAP error code (positive, see MAPErr*) and ErrorMessage its name —
	// or with a TCAP Reject / a ReturnError without a code — ErrorCode is
	// ErrCodeRemoteError.
	StatusRemoteError = "REMOTE_ERROR"
)

// Gateway-private failure codes in Message.ErrorCode. They are negative so
// they never collide with MAP error codes (3GPP TS 29.002 MAP-ErrorCodes, positive).
const (
	ErrCodeTimeout      int32 = -1
	ErrCodeAborted      int32 = -2
	ErrCodeRemoteError  int32 = -3
	ErrCodeRejected     int32 = -4
	ErrCodeUnclassified int32 = -99
)

// MAP error codes (3GPP TS 29.002, MAP-ErrorCodes) that SendRoutingInfoForSM
// and MT-ForwardSM can return, carried in Message.ErrorCode with
// StatusRemoteError. The gateway passes any code through; these are the ones
// an SMSC's retry logic normally distinguishes.
const (
	MAPErrUnknownSubscriber         int32 = 1
	MAPErrUnidentifiedSubscriber    int32 = 5
	MAPErrAbsentSubscriberSM        int32 = 6
	MAPErrIllegalSubscriber         int32 = 9
	MAPErrTeleserviceNotProvisioned int32 = 11
	MAPErrIllegalEquipment          int32 = 12
	MAPErrCallBarred                int32 = 13
	MAPErrFacilityNotSupported      int32 = 21
	MAPErrSubscriberBusyForMTSMS    int32 = 31
	MAPErrSMDeliveryFailure         int32 = 32
	MAPErrSystemFailure             int32 = 34
	MAPErrDataMissing               int32 = 35
	MAPErrUnexpectedDataValue       int32 = 36
)

// Causes that qualify an error the application refuses a request with
// (TS 29.002 §7.6.1, MAP-ER-DataTypes): RejectCause in an answer,
// ErrorCause on the wire.
const (
	// unknownSubscriber's diagnostic, which it may go without. TS 29.002
	// names gprs-eps-subscription-unknown for a sendAuthenticationInfo from
	// an SGSN for a subscriber without GPRS (figure 25.5/6).
	CauseIMSIUnknown                = "imsi-unknown"
	CauseGPRSEPSSubscriptionUnknown = "gprs-eps-subscription-unknown"
	// roamingNotAllowed's cause, which it always carries: without one,
	// plmn-roaming-not-allowed (TS 29.002 §8.1.2.3).
	CausePLMNRoamingNotAllowed       = "plmn-roaming-not-allowed"
	CauseOperatorDeterminedBarring   = "operator-determined-barring"
	CauseSupportedRATTypesNotAllowed = "supported-rat-types-not-allowed"
)

// checkRejectCause reports whether cause may qualify the error code.
func checkRejectCause(code int32, cause string) error {
	switch cause {
	case "":
		return nil
	case CauseIMSIUnknown, CauseGPRSEPSSubscriptionUnknown:
		if code == MAPErrUnknownSubscriber {
			return nil
		}
	case CausePLMNRoamingNotAllowed, CauseOperatorDeterminedBarring, CauseSupportedRATTypesNotAllowed:
		if code == MAPErrRoamingNotAllowed {
			return nil
		}
	}
	return fmt.Errorf("reject cause %q does not qualify error %d", cause, code)
}

// AddressString is the object form of an address parameter. Nature and
// NumberingPlan are the TS 29.002 AddressString nature-of-address and
// numbering-plan indicators, coded as there: Nature 1 = international,
// 2 = national significant, 4 = subscriber number (not the SCCP global
// title coding, where international is 4); NumberingPlan 1 = ISDN/E.164.
// A bare digit string in Parameters means international / ISDN-E.164.
type AddressString struct {
	Digits        string `json:"digits"`
	Nature        uint8  `json:"nature"`
	NumberingPlan uint8  `json:"numbering_plan"`
}

// E164 returns an international ISDN/E.164 AddressString.
func E164(digits string) AddressString {
	return AddressString{Digits: digits, Nature: 1, NumberingPlan: 1}
}
