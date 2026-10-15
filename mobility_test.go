// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: Apache-2.0

package ss7client

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// gatewayTurn sends a location update turn the way the gateway does: a NATS
// request on map.ul. A nil reply is no answer.
func gatewayTurn(t *testing.T, nc *nats.Conn, turn *Message) *Message {
	t.Helper()
	data, _ := json.Marshal(turn)
	if turn.LocationUpdate.Action == ULEnd {
		if err := nc.Publish(SubjectLocationUpdate, data); err != nil {
			t.Fatal(err)
		}
		return nil
	}
	m, err := nc.Request(SubjectLocationUpdate, data, 2*time.Second)
	if err != nil {
		t.Fatalf("turn %s: %v", turn.LocationUpdate.Action, err)
	}
	var reply Message
	if err := json.Unmarshal(m.Data, &reply); err != nil {
		t.Fatal(err)
	}
	return &reply
}

func TestServeLocationUpdates(t *testing.T) {
	nc := startNATS(t)
	c := newClient(t, nc)
	turns := make(chan *LocationUpdateTurn, 8)
	sub, err := c.ServeLocationUpdates("hlr", func(ctx context.Context, turn *LocationUpdateTurn) LocationUpdateAnswer {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the handler's context has no deadline")
		}
		turns <- turn
		switch turn.Action {
		case ULUpdateLocation:
			return LocationUpdateAnswer{Insert: &SubscriberData{MSISDN: &AddressString{Digits: "905312345678", Nature: 1, NumberingPlan: 1},
				Category: "0a", SubscriberStatus: "serviceGranted", Teleservices: []string{"11", "21"}}}
		case ULInsertResult:
			return LocationUpdateAnswer{Accept: true}
		}
		return LocationUpdateAnswer{}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	by := time.Now().Add(time.Second)
	reply := gatewayTurn(t, nc, &Message{CorrelationID: "c1", IMSI: "286010000000123", NodeID: "n1",
		LocationUpdate: &LocationUpdate{DialogueID: "d1", Action: ULUpdateLocation, AnswerBy: &by,
			VLRNumber: &AddressString{Digits: "905321000002", Nature: 1, NumberingPlan: 1}}})
	turn := <-turns
	if turn.IMSI != "286010000000123" || turn.DialogueID != "d1" || turn.Update.VLRNumber.Digits != "905321000002" {
		t.Fatalf("turn %+v", turn)
	}
	if reply.LocationUpdate.Action != ULInsert || reply.SubscriberData == nil || reply.SubscriberData.Category != "0a" ||
		reply.LocationUpdate.DialogueID != "d1" || reply.CorrelationID != "c1" {
		t.Fatalf("answer %+v / %+v", reply.LocationUpdate, reply.SubscriberData)
	}

	reply = gatewayTurn(t, nc, &Message{CorrelationID: "c2", IMSI: "286010000000123", InsertResult: &InsertResult{UnsupportedTeleservices: []string{"21"}},
		LocationUpdate: &LocationUpdate{DialogueID: "d1", Action: ULInsertResult, AnswerBy: &by}})
	if turn = <-turns; turn.Insert == nil || turn.Insert.UnsupportedTeleservices[0] != "21" {
		t.Fatalf("insert result turn %+v", turn)
	}
	if reply.LocationUpdate.Action != ULAccept || reply.SubscriberData != nil {
		t.Fatalf("answer %+v", reply.LocationUpdate)
	}

	// An end is told, not answered.
	gatewayTurn(t, nc, &Message{Status: StatusAborted, ErrorCode: ErrCodeAborted,
		LocationUpdate: &LocationUpdate{DialogueID: "d2", Action: ULEnd, AnswerBy: &by, Detail: "dialogue aborted by the VLR"}})
	if turn = <-turns; turn.Action != ULEnd || turn.Status != StatusAborted || turn.Detail != "dialogue aborted by the VLR" {
		t.Fatalf("end turn %+v", turn)
	}
}

// What the gateway could not carry out is answered as a refusal with
// systemFailure, saying why, rather than left to the gateway's timer.
func TestServeLocationUpdates_BadAnswers(t *testing.T) {
	for name, tc := range map[string]struct {
		answer LocationUpdateAnswer
		code   int32
		detail string
	}{
		"nothing":                   {LocationUpdateAnswer{}, MAPErrSystemFailure, "not one Insert"},
		"insert and accept":         {LocationUpdateAnswer{Insert: &SubscriberData{}, Accept: true}, MAPErrSystemFailure, "not one Insert"},
		"callBarred":                {LocationUpdateAnswer{Reject: MAPErrCallBarred}, MAPErrSystemFailure, "updateLocation error"},
		"invalid data":              {LocationUpdateAnswer{Insert: &SubscriberData{Category: "0a0b"}}, MAPErrSystemFailure, "Category"},
		"ODB without the status":    {LocationUpdateAnswer{Insert: &SubscriberData{ODB: &ODB{}}}, MAPErrSystemFailure, "ODB"},
		"roamingNotAllowed is fine": {LocationUpdateAnswer{Reject: MAPErrRoamingNotAllowed}, MAPErrRoamingNotAllowed, ""},
		"ODB is fine": {LocationUpdateAnswer{Reject: MAPErrRoamingNotAllowed, RejectCause: CauseOperatorDeterminedBarring},
			MAPErrRoamingNotAllowed, ""},
		"a cause of another error": {LocationUpdateAnswer{Reject: MAPErrRoamingNotAllowed, RejectCause: CauseIMSIUnknown},
			MAPErrSystemFailure, "does not qualify"},
		"an unknown cause": {LocationUpdateAnswer{Reject: MAPErrUnknownSubscriber, RejectCause: "imsi"},
			MAPErrSystemFailure, "does not qualify"},
		"a cause with an insert": {LocationUpdateAnswer{Insert: &SubscriberData{}, RejectCause: CauseIMSIUnknown},
			MAPErrSystemFailure, "does not qualify"},
	} {
		t.Run(name, func(t *testing.T) {
			nc := startNATS(t)
			c := newClient(t, nc)
			sub, err := c.ServeLocationUpdates("", func(context.Context, *LocationUpdateTurn) LocationUpdateAnswer { return tc.answer })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sub.Unsubscribe() })
			reply := gatewayTurn(t, nc, &Message{LocationUpdate: &LocationUpdate{DialogueID: "d", Action: ULUpdateLocation}})
			u := reply.LocationUpdate
			if u.Action != ULReject || u.Error != tc.code || !strings.Contains(u.Detail, tc.detail) {
				t.Fatalf("answer %+v", u)
			}
			if u.Error == tc.answer.Reject && u.ErrorCause != tc.answer.RejectCause {
				t.Fatalf("cause %q, want %q", u.ErrorCause, tc.answer.RejectCause)
			}
		})
	}
}

func TestCancelLocation(t *testing.T) {
	nc := startNATS(t)
	c := newClient(t, nc)
	fakeGateway(t, nc, SubjectCancelLocationRequest, SubjectCancelLocationResponse, func(req *Message) *Message {
		if req.IMSI != "286010000000123" || req.Parameters[ParamVLRNumber] != "905321000002" ||
			req.CancelLocation == nil || req.CancelLocation.Type != CancellationSubscriptionWithdraw ||
			req.CancelLocation.LMSI != "01020304" || req.MessageType != MsgTypeMAPCancelLocation {
			t.Errorf("request %+v / %+v", req, req.CancelLocation)
		}
		if req.DPC == 77 {
			return &Message{Status: StatusRemoteError, ErrorCode: MAPErrUnexpectedDataValue, ErrorMessage: "unexpectedDataValue"}
		}
		return &Message{}
	})
	req := CancelLocationRequest{IMSI: "286010000000123", VLRNumber: "905321000002", LMSI: "01020304", Type: CancellationSubscriptionWithdraw}
	if err := c.CancelLocation(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.VLRPointCode = 77
	var gwErr *GatewayError
	if err := c.CancelLocation(context.Background(), req); !errors.As(err, &gwErr) || gwErr.Code != MAPErrUnexpectedDataValue {
		t.Fatalf("err %v", err)
	}
	for name, bad := range map[string]CancelLocationRequest{
		"no VLR":       {IMSI: "286010000000123"},
		"no IMSI":      {VLRNumber: "905321000002"},
		"short LMSI":   {IMSI: "286010000000123", VLRNumber: "905321000002", LMSI: "0102"},
		"type":         {IMSI: "286010000000123", VLRNumber: "905321000002", Type: "initialAttachProcedure"},
		"IMSI letters": {IMSI: "28601000000012x", VLRNumber: "905321000002"},
	} {
		if err := c.CancelLocation(context.Background(), bad); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestProvideRoamingNumber(t *testing.T) {
	nc := startNATS(t)
	c := newClient(t, nc)
	msc := E164("905321000001")
	fakeGateway(t, nc, SubjectPRNRequest, SubjectPRNResponse, func(req *Message) *Message {
		e := req.RoamingNumberEnquiry
		if e == nil || e.MSCNumber.Digits != "905321000001" || e.CallReferenceNumber != "abcd" || !e.ORInterrogation {
			t.Errorf("request %+v", e)
		}
		rn, vmsc := E164("905300099999"), E164("905321000001")
		return &Message{RoamingNumberEnquiry: &RoamingNumberEnquiry{RoamingNumber: &rn, VMSCAddress: &vmsc}}
	})
	res, err := c.ProvideRoamingNumber(context.Background(), PRNRequest{IMSI: "286010000000123", VLRNumber: "905321000002",
		Enquiry: RoamingNumberEnquiry{MSCNumber: &msc, CallReferenceNumber: "abcd", ORInterrogation: true}})
	if err != nil {
		t.Fatal(err)
	}
	if res.RoamingNumber.Digits != "905300099999" || res.VMSCAddress.Digits != "905321000001" {
		t.Fatalf("result %+v", res)
	}
	rn := E164("1")
	for name, e := range map[string]RoamingNumberEnquiry{
		"no MSC number":   {},
		"answer field":    {MSCNumber: &msc, RoamingNumber: &rn},
		"bearer protocol": {MSCNumber: &msc, BearerCapability: &SignalInfo{ProtocolID: 5, Data: "a0"}},
		"bearer data":     {MSCNumber: &msc, BearerCapability: &SignalInfo{ProtocolID: 1}},
		"call reference":  {MSCNumber: &msc, CallReferenceNumber: "001122334455667788"},
	} {
		_, err := c.ProvideRoamingNumber(context.Background(), PRNRequest{IMSI: "286010000000123", VLRNumber: "905321000002", Enquiry: e})
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestInsertSubscriberData(t *testing.T) {
	nc := startNATS(t)
	c := newClient(t, nc)
	fakeGateway(t, nc, SubjectISDRequest, SubjectISDResponse, func(req *Message) *Message {
		if req.SubscriberData == nil || len(req.SubscriberData.CallBarring) != 1 {
			t.Errorf("request %+v", req.SubscriberData)
		}
		return &Message{InsertResult: &InsertResult{UnsupportedSSCodes: []string{"92"}}}
	})
	res, err := c.InsertSubscriberData(context.Background(), ISDRequest{IMSI: "286010000000123", VLRNumber: "905321000002",
		Data: SubscriberData{CallBarring: []CallBarringInfo{{SSCode: "92", Features: []CallBarringFeature{{Status: SSStatus{Provisioned: true, Active: true}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result.UnsupportedSSCodes[0] != "92" {
		t.Fatalf("result %+v", res.Result)
	}
	for name, d := range map[string]SubscriberData{
		"bad status":        {SubscriberStatus: "barred"},
		"bad ODB name":      {SubscriberStatus: "operatorDeterminedBarring", ODB: &ODB{General: []string{"allCalls"}}},
		"no features":       {Forwarding: []ForwardingInfo{{SSCode: "21"}}},
		"no reply 4":        {Forwarding: []ForwardingInfo{{SSCode: "2a", Features: []ForwardingFeature{{NoReplyTime: 4}}}}},
		"zone code":         {RegionalSubscription: []string{"01"}},
		"both options":      {SSData: []SSData{{SSCode: "12", CLIRestriction: "permanent", OverrideCategory: "overrideEnabled"}}},
		"basic kind":        {SSData: []SSData{{SSCode: "12", BasicServices: []BasicService{{Kind: "bearer", Code: "11"}}}}},
		"access":            {AccessRestriction: []string{"lteNotAllowed"}},
		"21 teleservices":   {Teleservices: make([]string, 21)},
		"long SS code":      {SSData: []SSData{{SSCode: "1212"}}},
		"forwarding reason": {Forwarding: []ForwardingInfo{{SSCode: "29", Features: []ForwardingFeature{{Options: &ForwardingOptions{Reason: "x"}}}}}},
	} {
		_, err := c.InsertSubscriberData(context.Background(), ISDRequest{IMSI: "286010000000123", VLRNumber: "905321000002", Data: d})
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The JSON field names are the contract with non-Go applications.
func TestMobilityWire(t *testing.T) {
	msg := &Message{
		LocationUpdate: &LocationUpdate{DialogueID: "d", Action: ULUpdateLocation, LMSI: "01020304",
			VLRCapability: &VLRCapability{SupportedCAMELPhases: []int{1, 2}, SupportedRATTypes: []string{"utran"}}},
		SubscriberData: &SubscriberData{Category: "0a", Forwarding: []ForwardingInfo{{SSCode: "2a",
			Features: []ForwardingFeature{{Status: SSStatus{Provisioned: true}, NoReplyTime: 20}}}},
			ODB: &ODB{General: []string{"allOG-CallsBarred"}}, RoamingRestrictedUnsupportedFeature: true},
		InsertResult:         &InsertResult{UnsupportedSSCodes: []string{"12"}},
		CancelLocation:       &CancelLocation{Type: CancellationUpdateProcedure},
		RoamingNumberEnquiry: &RoamingNumberEnquiry{BearerCapability: &SignalInfo{ProtocolID: 1, Data: "a0"}},
	}
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"location_update":{"dialogue_id":"d","action":"update_location","lmsi":"01020304"`,
		`"vlr_capability":{"supported_camel_phases":[1,2],"supported_rat_types":["utran"]}`,
		`"subscriber_data":{"category":"0a","forwarding":[{"ss_code":"2a","features":[{"status":{"provisioned":true},"no_reply_time":20}]}]`,
		`"odb":{"general":["allOG-CallsBarred"]},"roaming_restricted_unsupported_feature":true`,
		`"insert_result":{"unsupported_ss_codes":["12"]}`, `"cancel_location":{"type":"updateProcedure"}`,
		`"roaming_number_enquiry":{"bearer_capability":{"protocol_id":1,"data":"a0"}}`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("%s not in %s", key, b)
		}
	}
	if n := ODBGeneralNames(); len(n) != 29 || n[3] != "premiumRateInformationOGCallsBarred" || n[6] != "interzonalOGCallsBarred" || n[28] != "registrationInternationalCF-Barred" {
		t.Errorf("ODB-GeneralData names out of bit order: %v", n)
	}
}
