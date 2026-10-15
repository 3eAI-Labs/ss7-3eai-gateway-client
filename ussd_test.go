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

func dcs(b byte) *byte { return &b }

// A network-initiated session: a request, the reply, a notify, release.
func TestUSSDNetworkInitiatedSession(t *testing.T) {
	nc := startNATS(t)
	fakeGateway(t, nc, SubjectUSSDStart, SubjectUSSDResponse, func(req *Message) *Message {
		u := req.USSD
		if req.MessageType != MsgTypeMAPUSSD || req.MSISDN != "905321112233" || u == nil ||
			u.Action != USSDRequest || u.Text != "Balance? 1=yes" || u.DCS != nil || u.SessionID != "" || u.Route != "billing" {
			t.Errorf("start = %+v ussd %+v", req, u)
		}
		return &Message{MessageType: MsgTypeMAPUSSD, NodeID: "node-1",
			USSD: &USSD{SessionID: "s-1", Action: USSDRequest, DCS: dcs(DCSGSM7), Data: []byte{0x31}, Text: "1"}}
	})
	var commands []*USSD
	fakeGateway(t, nc, SubjectUSSDCommand, SubjectUSSDResponse, func(req *Message) *Message {
		commands = append(commands, req.USSD)
		if req.ErrorCode != 0 {
			t.Errorf("command carries error_code %d", req.ErrorCode)
		}
		return &Message{MessageType: MsgTypeMAPUSSD, USSD: &USSD{SessionID: req.USSD.SessionID, Action: req.USSD.Action}}
	})
	c := newClient(t, nc)
	ctx := context.Background()

	a, err := c.StartUSSD(ctx, USSDStart{MSISDN: "905321112233", Route: "billing", Message: USSDText{Text: "Balance? 1=yes"}})
	if err != nil {
		t.Fatal(err)
	}
	if a.SessionID != "s-1" || a.Reply.Text != "1" || a.Reply.DCS != DCSGSM7 || string(a.Reply.Data) != "1" || a.Reply.Empty {
		t.Fatalf("answer = %+v reply %+v", a, a.Reply)
	}
	if _, err := c.USSDNotify(ctx, a.SessionID, USSDText{DCS: dcs(DCSUCS2), Data: []byte{0x00, 0x41}}); err != nil {
		t.Fatal(err)
	}
	if err := c.USSDRelease(ctx, a.SessionID, nil); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || commands[0].Action != USSDNotify || *commands[0].DCS != DCSUCS2 || string(commands[0].Data) != "\x00A" ||
		commands[1].Action != USSDRelease || commands[1].Text != "" || commands[1].SessionID != a.SessionID {
		t.Fatalf("commands = %+v %+v", commands[0], commands[1])
	}
}

// unstructuredSS-Request's result is optional: an empty reply is an answer,
// not an error. A notify start returns no reply.
func TestUSSDEmptyReplyAndNotifyStart(t *testing.T) {
	nc := startNATS(t)
	fakeGateway(t, nc, SubjectUSSDStart, SubjectUSSDResponse, func(req *Message) *Message {
		if req.USSD.Action != USSDNotify || req.IMSI != "286010000000123" {
			t.Errorf("start = %+v", req.USSD)
		}
		return &Message{MessageType: MsgTypeMAPUSSD, USSD: &USSD{SessionID: "s-2", Action: USSDNotify}}
	})
	fakeGateway(t, nc, SubjectUSSDCommand, SubjectUSSDResponse, func(req *Message) *Message {
		return &Message{MessageType: MsgTypeMAPUSSD, USSD: &USSD{SessionID: req.USSD.SessionID, Action: USSDRequest}}
	})
	c := newClient(t, nc)
	a, err := c.StartUSSD(context.Background(), USSDStart{IMSI: "286010000000123", Notify: true, Message: USSDText{Text: "Hi"}})
	if err != nil || a.Reply != nil {
		t.Fatalf("notify start: %+v %v", a, err)
	}
	r, err := c.USSDRequest(context.Background(), "s-2", USSDText{Text: "PIN?"})
	if err != nil || r.Reply == nil || !r.Reply.Empty {
		t.Fatalf("empty reply: %+v %v", r, err)
	}
}

// A subscriber-initiated session arrives on its route; the application
// rejects it; the end event says how it ended.
func TestUSSDSubscriberInitiated(t *testing.T) {
	nc := startNATS(t)
	var rejected *Message
	fakeGateway(t, nc, SubjectUSSDCommand, SubjectUSSDResponse, func(req *Message) *Message {
		rejected = req
		return &Message{MessageType: MsgTypeMAPUSSD, USSD: &USSD{SessionID: req.USSD.SessionID, Action: USSDReject}}
	})
	c := newClient(t, nc)
	events := make(chan *USSDEvent, 4)
	sub, err := c.SubscribeUSSD("balance", "ussd-app", func(e *USSDEvent) { events <- e })
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	publish := func(m Message) {
		b, _ := json.Marshal(m)
		if err := nc.Publish(SubjectUSSDEventPrefix+"balance", b); err != nil {
			t.Fatal(err)
		}
	}
	next := func() *USSDEvent {
		select {
		case e := <-events:
			return e
		case <-time.After(2 * time.Second):
			t.Fatal("no event")
			return nil
		}
	}
	publish(Message{MessageType: MsgTypeMAPUSSD, IMSI: "286010000000123", MSISDN: "905321112233",
		USSD: &USSD{SessionID: "s-7", Action: USSDBegin, Route: "balance", DCS: dcs(DCSGSM7),
			Data: []byte{0xAA, 0x98, 0x6C, 0x36, 0x02}, Text: "*123#"}})
	e := next()
	if e.Action != USSDBegin || e.Route != "balance" || e.IMSI != "286010000000123" || e.Message.Text != "*123#" || len(e.Message.Data) != 5 {
		t.Fatalf("begin = %+v message %+v", e, e.Message)
	}
	if err := c.USSDReject(context.Background(), e.SessionID, MAPErrUnknownAlphabet); err != nil {
		t.Fatal(err)
	}
	if rejected.USSD.Action != USSDReject || rejected.USSD.Error != 71 || rejected.ErrorCode != 0 || rejected.USSD.SessionID != "s-7" {
		t.Fatalf("reject = %+v ussd %+v", rejected, rejected.USSD)
	}

	publish(Message{MessageType: MsgTypeMAPUSSD, Status: StatusRemoteError, ErrorCode: MAPErrUSSDBusy, ErrorMessage: "ussd-Busy",
		USSD: &USSD{SessionID: "s-8", Action: USSDEnd}})
	if e = next(); e.Action != USSDEnd || e.Status != StatusRemoteError || e.Code != 72 || e.Detail != "ussd-Busy" || e.Message != nil {
		t.Fatalf("end = %+v", e)
	}
	publish(Message{MessageType: MsgTypeMAPUSSD, USSD: &USSD{SessionID: "s-9", Action: USSDEnd, Detail: "network"}})
	if e = next(); e.Status != "" || e.Code != 0 || e.Detail != "network" {
		t.Fatalf("released end = %+v", e)
	}
	// A begin whose message does not decode is delivered, to be rejected.
	publish(Message{MessageType: MsgTypeMAPUSSD, USSD: &USSD{SessionID: "s-10", Action: USSDBegin, DCS: dcs(0x0F)}})
	if e = next(); e.Message != nil || e.Detail == "" {
		t.Fatalf("undecodable begin = %+v", e)
	}
}

func TestUSSDInvalidRequests(t *testing.T) {
	nc := startNATS(t)
	sub, err := nc.SubscribeSync("map.ussd.>")
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, nc)
	ctx := context.Background()
	long := make([]rune, 183)
	for i := range long {
		long[i] = 'a'
	}
	start := func(s USSDStart) error { _, err := c.StartUSSD(ctx, s); return err }
	x := USSDText{Text: "x"}
	for name, err := range map[string]error{
		"no identity":       start(USSDStart{Message: x}),
		"both identities":   start(USSDStart{MSISDN: "1", IMSI: "2", Message: x}),
		"no text":           start(USSDStart{MSISDN: "1"}),
		"text and data":     start(USSDStart{MSISDN: "1", Message: USSDText{Text: "x", DCS: dcs(0x0F), Data: []byte{1}}}),
		"data without dcs":  start(USSDStart{MSISDN: "1", Message: USSDText{Data: []byte{1}}}),
		"183 characters":    start(USSDStart{MSISDN: "1", Message: USSDText{Text: string(long)}}),
		"161 octets":        start(USSDStart{MSISDN: "1", Message: USSDText{DCS: dcs(0x0F), Data: make([]byte, 161)}}),
		"MSISDN with +":     start(USSDStart{MSISDN: "+1", Message: x}),
		"IMSI of 16":        start(USSDStart{IMSI: "1234567890123456", Message: x}),
		"bad route":         start(USSDStart{MSISDN: "1", Route: "a.b", Message: x}),
		"no session":        func() error { _, err := c.USSDRequest(ctx, "", x); return err }(),
		"reject absent sub": c.USSDReject(ctx, "s", MAPErrAbsentSubscriber),
	} {
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	if _, err := c.SubscribeUSSD("a.b", "q", func(*USSDEvent) {}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("subscribe to route a.b: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	if m, err := sub.NextMsg(100 * time.Millisecond); err == nil {
		t.Fatalf("an invalid request reached NATS on %s", m.Subject)
	}
}

// A command without a context deadline gets DefaultUSSDTimeout, not the 40 s
// of the other operations.
func TestUSSDDefaultTimeout(t *testing.T) {
	nc := startNATS(t)
	c := newClient(t, nc, WithUSSDTimeout(150*time.Millisecond))
	start := time.Now()
	_, err := c.USSDRequest(context.Background(), "s", USSDText{Text: "x"})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("err %v after %s", err, time.Since(start))
	}
}

// The USSD fields are wire contract like the rest of Message.
func TestUSSDWire(t *testing.T) {
	data, err := json.Marshal(Message{MessageType: MsgTypeMAPUSSD, USSD: &USSD{SessionID: "n/1", Action: USSDReject,
		Route: "r", DCS: dcs(0x0F), Data: []byte{0x31}, Text: "1", Error: 34, Detail: "network"}})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	const want = `{"session_id":"n/1","action":"reject","route":"r","dcs":15,"data":"MQ==","text":"1","error":34,"detail":"network"}`
	if string(raw["ussd"]) != want {
		t.Fatalf("got %s\nwant %s", raw["ussd"], want)
	}
	if _, ok := raw["error_code"]; ok {
		t.Fatal("a reject carries error_code")
	}
	for got, want := range map[int32]int32{MAPErrUnknownAlphabet: 71, MAPErrUSSDBusy: 72} {
		if got != want {
			t.Errorf("error code %d, want %d", got, want)
		}
	}
	for got, want := range map[byte]byte{DCSGSM7: 0x0F, DCSUCS2: 0x48} {
		if got != want {
			t.Errorf("DCS %#x, want %#x", got, want)
		}
	}
}
