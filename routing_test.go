// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: Apache-2.0

package ss7client

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestSendRoutingInfo_Success(t *testing.T) {
	nc := startNATS(t)
	fakeGateway(t, nc, SubjectRoutingRequest, SubjectRoutingResponse, func(req *Message) *Message {
		if req.MessageType != MsgTypeMAPRouting || req.MSISDN != "905321112233" || req.GlobalTitle != "" {
			t.Errorf("request = %s msisdn=%q gt=%q", req.MessageType, req.MSISDN, req.GlobalTitle)
		}
		if len(req.Parameters) != 0 {
			t.Errorf("parameters = %v, want none for a basic call", req.Parameters)
		}
		return &Message{
			MessageType: MsgTypeMAPRouting,
			NodeID:      "node-1",
			IMSI:        "286010000000123",
			RoutingInfo: &RoutingInfo{RoamingNumber: &AddressString{Digits: "905329990001", Nature: 1, NumberingPlan: 1}, VMSCAddress: "905320000001"},
			SubscriberInfo: &SubscriberInfo{
				State: &SubscriberState{State: "assumedIdle"},
			},
		}
	})
	c := newClient(t, nc)

	res, err := c.SendRoutingInfo(context.Background(), RoutingRequest{MSISDN: "905321112233"})
	if err != nil {
		t.Fatalf("SRI: %v", err)
	}
	if res.IMSI != "286010000000123" || res.Routing.RoamingNumber.Digits != "905329990001" ||
		res.Routing.VMSCAddress != "905320000001" || res.NodeID != "node-1" {
		t.Fatalf("result = %+v routing = %+v", res, res.Routing)
	}
	if res.SubscriberInfo == nil || res.SubscriberInfo.State.State != "assumedIdle" {
		t.Fatalf("subscriber info = %+v", res.SubscriberInfo)
	}
}

func TestSendRoutingInfo_Forwarded(t *testing.T) {
	nc := startNATS(t)
	fakeGateway(t, nc, SubjectRoutingRequest, SubjectRoutingResponse, func(req *Message) *Message {
		if req.DPC != 0x0102 || req.GlobalTitle != "905320000009" {
			t.Errorf("request = %+v", req)
		}
		return &Message{MessageType: MsgTypeMAPRouting, RoutingInfo: &RoutingInfo{
			Forwarding: &ForwardingData{ForwardedToNumber: &AddressString{Digits: "905551112233", Nature: 1, NumberingPlan: 1},
				Options: &ForwardingOptions{NotifyCallingParty: true, Reason: "busy"}},
		}}
	})
	c := newClient(t, nc)

	res, err := c.SendRoutingInfo(context.Background(), RoutingRequest{
		MSISDN: "905321112233", HLRGlobalTitle: "905320000009", HLRPointCode: 0x0102,
	})
	if err != nil {
		t.Fatalf("SRI: %v", err)
	}
	if f := res.Routing.Forwarding; f == nil || f.ForwardedToNumber.Digits != "905551112233" || f.Options.Reason != "busy" {
		t.Fatalf("forwarding = %+v", res.Routing.Forwarding)
	}
}

func TestSendRoutingInfo_GatewayFailures(t *testing.T) {
	nc := startNATS(t)
	fakeGateway(t, nc, SubjectRoutingRequest, SubjectRoutingResponse, func(req *Message) *Message {
		if req.MSISDN == "1" {
			return &Message{MessageType: MsgTypeMAPRouting, Status: StatusRemoteError,
				ErrorCode: MAPErrAbsentSubscriber, ErrorMessage: "absentSubscriber"}
		}
		return &Message{MessageType: MsgTypeMAPRouting} // success without the answer
	})
	c := newClient(t, nc)
	ctx := context.Background()

	_, err := c.SendRoutingInfo(ctx, RoutingRequest{MSISDN: "1"})
	var ge *GatewayError
	if !errors.As(err, &ge) || ge.Status != StatusRemoteError || ge.Code != MAPErrAbsentSubscriber {
		t.Errorf("SRI error = %v, want REMOTE_ERROR 27", err)
	}
	if _, err = c.SendRoutingInfo(ctx, RoutingRequest{MSISDN: "2"}); !errors.Is(err, errNoRoutingInfo) {
		t.Errorf("SRI without routing_info: err = %v", err)
	}
}

func TestInvalidRoutingRequestIsNotPublished(t *testing.T) {
	nc := startNATS(t)
	sub, err := nc.SubscribeSync(SubjectRoutingRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	c := newClient(t, nc)
	sri := func(r RoutingRequest) error { _, err := c.SendRoutingInfo(context.Background(), r); return err }

	cases := map[string]error{
		"no MSISDN":         sri(RoutingRequest{}),
		"MSISDN with +":     sri(RoutingRequest{MSISDN: "+905321112233"}),
		"MSISDN too long":   sri(RoutingRequest{MSISDN: "12345678901234567"}),
		"HLR GT not digits": sri(RoutingRequest{MSISDN: "1", HLRGlobalTitle: "90 53"}),
	}
	for name, err := range cases {
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	// The request goes through the client's own connection: once a
	// round trip on it has completed, anything published before would have
	// been delivered.
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	if m, err := sub.NextMsg(100 * time.Millisecond); err == nil {
		t.Fatalf("an invalid request reached NATS: %s", m.Data)
	}
}

// The error codes are the specification's (TS 29.002 MAP-ErrorCodes,
// CODE local:N), pinned by value.
func TestRoutingErrorCodes(t *testing.T) {
	for got, want := range map[int32]int32{
		MAPErrBearerServiceNotProvisioned: 10, MAPErrForwardingViolation: 14, MAPErrCUGReject: 15,
		MAPErrAbsentSubscriber: 27, MAPErrNumberChanged: 44, MAPErrBusySubscriber: 45,
		MAPErrNoSubscriberReply: 46, MAPErrORNotAllowed: 48,
	} {
		if got != want {
			t.Errorf("error code %d, want %d", got, want)
		}
	}
}

// The request on the wire, as a non-Go gateway would read it.
func TestRoutingRequestWire(t *testing.T) {
	nc := startNATS(t)
	sub, err := nc.SubscribeSync(SubjectRoutingRequest)
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, nc)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	go func() { _, _ = c.SendRoutingInfo(ctx, RoutingRequest{MSISDN: "905321112233", HLRPointCode: 0x0102}) }()
	m, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(m.Data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["message_type"] != "MAP_ROUTING" || raw["msisdn"] != "905321112233" || raw["dpc"] != float64(0x0102) ||
		raw["direction"] != "INBOUND" || raw["correlation_id"] == "" {
		t.Fatalf("request = %s", m.Data)
	}
	for _, k := range []string{"parameters", "global_title", "imsi", "routing_info"} {
		if _, ok := raw[k]; ok {
			t.Errorf("request carries %q: %s", k, m.Data)
		}
	}
}

// Answers as a gateway in another language would write them.
func TestRoutingAnswerFromJSON(t *testing.T) {
	var minimal Message
	if err := json.Unmarshal([]byte(`{"message_type":"MAP_ROUTING","imsi":"286010000000123","routing_info":{}}`), &minimal); err != nil {
		t.Fatal(err)
	}
	if minimal.RoutingInfo == nil {
		t.Fatal(`"routing_info":{} decoded as nil`)
	}
	var fwd Message
	if err := json.Unmarshal([]byte(`{"routing_info":{"forwarding":{"forwarded_to_number":{"digits":"*21#","nature":0,"numbering_plan":0}}}}`), &fwd); err != nil {
		t.Fatal(err)
	}
	f := fwd.RoutingInfo.Forwarding
	if f.Options != nil || f.ForwardedToNumber.Digits != "*21#" || f.ForwardedToNumber.Nature != 0 {
		t.Fatalf("forwarding = %+v", f)
	}
}

// The routing fields are wire contract like the rest of Message.
func TestRoutingInfoWireFieldNames(t *testing.T) {
	fwd := &ForwardingData{ForwardedToNumber: &AddressString{Digits: "1", Nature: 0, NumberingPlan: 1}, ForwardedToSubaddress: "a0",
		Options: &ForwardingOptions{NotifyForwardingParty: true, RedirectingPresentation: true,
			NotifyCallingParty: true, Reason: "unconditional"}}
	ri := &RoutingInfo{
		RoamingNumber: &AddressString{Digits: "2", Nature: 1, NumberingPlan: 1}, Forwarding: fwd,
		CAMEL:       &CAMELRouting{Forwarding: &ForwardingData{Options: &ForwardingOptions{Reason: "noReply"}}, SubscriptionInfo: "a000"},
		VMSCAddress: "4", MSISDN: "5", ForwardingInterrogationRequired: true,
		NumberPortabilityStatus: "ownNumberPortedOut",
		BasicService:            &BasicService{Kind: "ext-Teleservice", Code: "11"},
		SSCodes:                 []string{"21", "29"},
	}
	data, err := json.Marshal(struct {
		R *RoutingInfo `json:"routing_info"`
	}{ri})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"routing_info":{"roaming_number":{"digits":"2","nature":1,"numbering_plan":1},` +
		`"forwarding":{"forwarded_to_number":{"digits":"1","nature":0,"numbering_plan":1},"forwarded_to_subaddress":"a0",` +
		`"options":{"notify_forwarding_party":true,"redirecting_presentation":true,"notify_calling_party":true,"reason":"unconditional"}},` +
		`"camel":{"forwarding":{"options":{"reason":"noReply"}},"subscription_info":"a000"},` +
		`"vmsc_address":"4","msisdn":"5","forwarding_interrogation_required":true,` +
		`"number_portability_status":"ownNumberPortedOut","basic_service":{"kind":"ext-Teleservice","code":"11"},` +
		`"ss_codes":["21","29"]}}`
	if string(data) != want {
		t.Fatalf("wire format changed:\n got %s\nwant %s", data, want)
	}
	full, err := json.Marshal(Message{MessageType: MsgTypeMAPRouting, RoutingInfo: ri})
	if err != nil {
		t.Fatal(err)
	}
	var back Message
	if err := json.Unmarshal(full, &back); err != nil || back.RoutingInfo.Forwarding.Options.Reason != "unconditional" {
		t.Fatalf("round trip: %v %+v", err, back.RoutingInfo)
	}
}
