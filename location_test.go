// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: Apache-2.0

package ss7client

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/nats-io/nats.go"
)

func TestAnyTimeInterrogation_Success(t *testing.T) {
	nc := startNATS(t)
	age := 3
	fakeGateway(t, nc, SubjectATIRequest, SubjectATIResponse, func(req *Message) *Message {
		if req.MessageType != MsgTypeMAPATI || req.MSISDN != "905321112233" || req.IMSI != "" {
			t.Errorf("request = %s msisdn=%q imsi=%q", req.MessageType, req.MSISDN, req.IMSI)
		}
		if ri := req.RequestedInfo; ri == nil || !ri.Location || !ri.State || ri.Domain != "" {
			t.Errorf("requested_info = %+v", req.RequestedInfo)
		}
		if len(req.Parameters) != 0 {
			t.Errorf("parameters = %v, want none", req.Parameters)
		}
		return &Message{
			MessageType: MsgTypeMAPATI,
			NodeID:      "node-1",
			SubscriberInfo: &SubscriberInfo{
				Location: &Location{
					AgeMinutes:   &age,
					VLRNumber:    "905320000001",
					CellGlobalID: &CellGlobalID{MCC: "286", MNC: "01", LAC: 0x1234, CI: 0},
				},
				State: &SubscriberState{State: "assumedIdle"},
			},
		}
	})
	c := newClient(t, nc)

	res, err := c.AnyTimeInterrogation(context.Background(), ATIRequest{
		MSISDN:    "905321112233",
		Requested: RequestedInfo{Location: true, State: true},
	})
	if err != nil {
		t.Fatalf("ATI: %v", err)
	}
	loc := res.Info.Location
	if loc == nil || loc.VLRNumber != "905320000001" || *loc.AgeMinutes != 3 || loc.CellGlobalID.LAC != 0x1234 {
		t.Fatalf("location = %+v", loc)
	}
	if res.Info.State.State != "assumedIdle" || res.NodeID != "node-1" {
		t.Fatalf("result = %+v", res)
	}
}

func TestProvideSubscriberInfo_Success(t *testing.T) {
	nc := startNATS(t)
	fakeGateway(t, nc, SubjectPSIRequest, SubjectPSIResponse, func(req *Message) *Message {
		if req.MessageType != MsgTypeMAPPSI || req.IMSI != "286010000000123" || req.DPC != 0x0203 {
			t.Errorf("request = %s imsi=%q dpc=%#x", req.MessageType, req.IMSI, req.DPC)
		}
		if ri := req.RequestedInfo; ri == nil || !ri.IMEI || ri.Domain != DomainPS {
			t.Errorf("requested_info = %+v", req.RequestedInfo)
		}
		if vlr := addressParam(t, req, ParamVLRNumber); vlr["digits"] != "905320000001" {
			t.Errorf("vlr_number = %v", vlr)
		}
		return &Message{
			MessageType:    MsgTypeMAPPSI,
			SubscriberInfo: &SubscriberInfo{IMEI: "490154203237518"},
		}
	})
	c := newClient(t, nc)

	res, err := c.ProvideSubscriberInfo(context.Background(), PSIRequest{
		IMSI:         "286010000000123",
		VLRNumber:    E164("905320000001"),
		Requested:    RequestedInfo{IMEI: true, Domain: DomainPS},
		VLRPointCode: 0x0203,
	})
	if err != nil {
		t.Fatalf("PSI: %v", err)
	}
	if res.Info.IMEI != "490154203237518" {
		t.Fatalf("result = %+v", res.Info)
	}
}

func TestLocationFailures(t *testing.T) {
	nc := startNATS(t)
	fakeGateway(t, nc, SubjectATIRequest, SubjectATIResponse, func(req *Message) *Message {
		if req.IMSI == "1" {
			return &Message{MessageType: MsgTypeMAPATI, Status: StatusRemoteError,
				ErrorCode: MAPErrATINotAllowed, ErrorMessage: "ati-NotAllowed"}
		}
		return &Message{MessageType: MsgTypeMAPATI} // success without the answer
	})
	fakeGateway(t, nc, SubjectPSIRequest, SubjectPSIResponse, func(req *Message) *Message {
		return &Message{MessageType: MsgTypeMAPPSI, Status: StatusRejected,
			ErrorCode: ErrCodeRejected, ErrorMessage: "PSI request: no capacity"}
	})
	c := newClient(t, nc)
	ctx := context.Background()

	_, err := c.AnyTimeInterrogation(ctx, ATIRequest{IMSI: "1", HLRGlobalTitle: "2", Requested: RequestedInfo{State: true}})
	var ge *GatewayError
	if !errors.As(err, &ge) || ge.Status != StatusRemoteError || ge.Code != MAPErrATINotAllowed {
		t.Errorf("ATI error = %v, want REMOTE_ERROR 49", err)
	}
	_, err = c.AnyTimeInterrogation(ctx, ATIRequest{IMSI: "2", HLRGlobalTitle: "2", Requested: RequestedInfo{State: true}})
	if !errors.Is(err, errNoSubscriberInfo) {
		t.Errorf("ATI without subscriber_info: err = %v", err)
	}
	_, err = c.ProvideSubscriberInfo(ctx, PSIRequest{IMSI: "1", VLRNumber: E164("2"), Requested: RequestedInfo{State: true}})
	if !errors.As(err, &ge) || ge.Status != StatusRejected || ge.Code != ErrCodeRejected {
		t.Errorf("PSI error = %v, want REJECTED -4", err)
	}
}

func TestInvalidLocationRequestIsNotPublished(t *testing.T) {
	nc := startNATS(t)
	published := make(chan struct{}, 16)
	for _, s := range []string{SubjectATIRequest, SubjectPSIRequest} {
		sub, err := nc.Subscribe(s, func(*nats.Msg) { published <- struct{}{} })
		if err != nil {
			t.Fatal(err)
		}
		defer sub.Unsubscribe()
	}
	c := newClient(t, nc)
	ctx := context.Background()
	loc := RequestedInfo{Location: true}
	ati := func(r ATIRequest) error { _, err := c.AnyTimeInterrogation(ctx, r); return err }
	psi := func(r PSIRequest) error { _, err := c.ProvideSubscriberInfo(ctx, r); return err }

	cases := map[string]error{
		"ATI no identity":       ati(ATIRequest{Requested: loc}),
		"ATI both identities":   ati(ATIRequest{MSISDN: "1", IMSI: "2", Requested: loc}),
		"ATI nothing requested": ati(ATIRequest{MSISDN: "1"}),
		"ATI current location alone": ati(ATIRequest{MSISDN: "1",
			Requested: RequestedInfo{State: true, CurrentLocation: true}}),
		"ATI EPS location alone": ati(ATIRequest{MSISDN: "1",
			Requested: RequestedInfo{State: true, EPSLocation: true}}),
		"ATI unknown domain":    ati(ATIRequest{MSISDN: "1", Requested: RequestedInfo{State: true, Domain: "5gs"}}),
		"PSI no IMSI":           psi(PSIRequest{VLRNumber: E164("1"), Requested: loc}),
		"PSI no VLR":            psi(PSIRequest{IMSI: "1", Requested: loc}),
		"PSI MNP":               psi(PSIRequest{IMSI: "1", VLRNumber: E164("1"), Requested: RequestedInfo{MNP: true}}),
		"PSI nothing":           psi(PSIRequest{IMSI: "1", VLRNumber: E164("1")}),
		"PSI VLR with +":        psi(PSIRequest{IMSI: "1", VLRNumber: E164("+905320000001"), Requested: loc}),
		"ATI HLR GT not digits": ati(ATIRequest{MSISDN: "1", HLRGlobalTitle: "90 53", Requested: loc}),
		"ATI MSISDN with +":     ati(ATIRequest{MSISDN: "+905321112233", Requested: loc}),
	}
	for name, err := range cases {
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(published) != 0 {
		t.Fatalf("%d invalid requests reached NATS", len(published))
	}
}

// The location fields are wire contract like the rest of Message.
func TestSubscriberInfoWireFieldNames(t *testing.T) {
	age := 0
	m := Message{
		RequestedInfo: &RequestedInfo{Location: true, CurrentLocation: true, EPSLocation: true, State: true,
			Domain: DomainCS, IMEI: true, MSClassmark: true, MNP: true},
		SubscriberInfo: &SubscriberInfo{
			Location: &Location{AgeMinutes: &age, VLRNumber: "1", MSCNumber: "2",
				CellGlobalID:   &CellGlobalID{MCC: "286", MNC: "01", LAC: 1, CI: 2},
				LocationNumber: "aa", GeographicalInformation: "bb", GeodeticInformation: "cc",
				CurrentLocationRetrieved: true,
				EPS:                      &LocationEPS{ECGI: "dd", TAI: "ee", MMEName: "mme", AgeMinutes: &age}},
			State:        &SubscriberState{State: "netDetNotReachable", NotReachableReason: "imsiDetached"},
			LocationGPRS: &Location{SGSNNumber: "3", ServiceAreaID: &ServiceAreaID{MCC: "286", MNC: "01", LAC: 3, SAC: 4}, RoutingAreaID: "ff"},
			PSState:      &SubscriberState{State: "ps-Detached"},
			EPSState:     &SubscriberState{State: "ps-AttachedReachableForPaging"},
			LocationEPS:  &LocationEPS{ECGI: "11"},
			IMEI:         "4", MSClassmark2: "22",
			MNP: &MNPInfo{RouteingNumber: "5", IMSI: "6", MSISDN: "7", Status: "foreignNumberPortedIn"},
		},
	}
	m.SubscriberInfo.LocationGPRS.LocationArea = &LocationArea{MCC: "286", MNC: "01", LAC: 5}
	data, err := json.Marshal(struct {
		R *RequestedInfo  `json:"requested_info"`
		S *SubscriberInfo `json:"subscriber_info"`
	}{m.RequestedInfo, m.SubscriberInfo})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"requested_info":{"location":true,"current_location":true,"eps_location":true,"state":true,` +
		`"domain":"cs","imei":true,"ms_classmark":true,"mnp":true},` +
		`"subscriber_info":{"location":{"age_minutes":0,"vlr_number":"1","msc_number":"2",` +
		`"cell_global_id":{"mcc":"286","mnc":"01","lac":1,"ci":2},"location_number":"aa",` +
		`"geographical_information":"bb","geodetic_information":"cc","current_location_retrieved":true,` +
		`"eps":{"ecgi":"dd","tai":"ee","mme_name":"mme","age_minutes":0}},` +
		`"state":{"state":"netDetNotReachable","not_reachable_reason":"imsiDetached"},` +
		`"location_gprs":{"sgsn_number":"3","service_area_id":{"mcc":"286","mnc":"01","lac":3,"sac":4},` +
		`"location_area":{"mcc":"286","mnc":"01","lac":5},"routing_area_id":"ff"},` +
		`"ps_state":{"state":"ps-Detached"},"eps_state":{"state":"ps-AttachedReachableForPaging"},` +
		`"location_eps":{"ecgi":"11"},"imei":"4","ms_classmark2":"22",` +
		`"mnp":{"routeing_number":"5","imsi":"6","msisdn":"7","status":"foreignNumberPortedIn"}}}`
	if string(data) != want {
		t.Fatalf("wire format changed:\n got %s\nwant %s", data, want)
	}
	full, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back Message
	if err := json.Unmarshal(full, &back); err != nil || back.SubscriberInfo.MNP.Status != "foreignNumberPortedIn" {
		t.Fatalf("round trip: %v %+v", err, back.SubscriberInfo)
	}
}
