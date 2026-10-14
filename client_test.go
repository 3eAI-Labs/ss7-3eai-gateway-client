// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: Apache-2.0

package ss7client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
)

// startNATS runs an in-process NATS server for one test.
func startNATS(t *testing.T) *nats.Conn {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = -1
	srv := natstest.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// fakeGateway answers requests on subject with reply(req), published on the
// response subject the way the real gateway does. A nil reply drops the request.
func fakeGateway(t *testing.T, nc *nats.Conn, reqSubject, respSubject string, reply func(req *Message) *Message) {
	t.Helper()
	sub, err := nc.QueueSubscribe(reqSubject, "ss7-cluster", func(m *nats.Msg) {
		var req Message
		if err := json.Unmarshal(m.Data, &req); err != nil {
			t.Errorf("gateway: decode request: %v", err)
			return
		}
		resp := reply(&req)
		if resp == nil {
			return
		}
		resp.CorrelationID = req.CorrelationID
		resp.Direction = DirectionOutbound
		data, _ := json.Marshal(resp)
		if err := nc.Publish(respSubject, data); err != nil {
			t.Errorf("gateway: publish: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("gateway subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

func newClient(t *testing.T, nc *nats.Conn, opts ...Option) *Client {
	t.Helper()
	c, err := New(nc, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// addressParam decodes a parameter the way the gateway does: JSON object →
// map[string]interface{} with float64 numbers.
func addressParam(t *testing.T, req *Message, key string) map[string]interface{} {
	t.Helper()
	m, ok := req.Parameters[key].(map[string]interface{})
	if !ok {
		t.Fatalf("parameter %s: got %T, want object", key, req.Parameters[key])
	}
	return m
}

func TestSendRoutingInfoForSM_Success(t *testing.T) {
	nc := startNATS(t)
	fakeGateway(t, nc, SubjectSRIRequest, SubjectSRIResponse, func(req *Message) *Message {
		if req.MessageType != MsgTypeMAPSRI || req.Direction != DirectionInbound {
			t.Errorf("request type/direction = %s/%s", req.MessageType, req.Direction)
		}
		if req.MSISDN != "905321112233" {
			t.Errorf("msisdn = %q", req.MSISDN)
		}
		if req.MessageID == "" || req.CorrelationID == "" || req.MessageID == req.CorrelationID {
			t.Errorf("ids: message_id=%q correlation_id=%q", req.MessageID, req.CorrelationID)
		}
		if req.GlobalTitle != "905300000099" || req.DPC != 0x0102 {
			t.Errorf("HLR override: gt=%q dpc=%#x", req.GlobalTitle, req.DPC)
		}
		sca := addressParam(t, req, ParamServiceCentreAddress)
		if sca["digits"] != "905001112233" || sca["nature"] != float64(1) || sca["numbering_plan"] != float64(1) {
			t.Errorf("service_centre_address = %v", sca)
		}
		return &Message{
			MessageType: MsgTypeMAPSRI,
			IMSI:        "286010000000123",
			NodeID:      "node-2",
			Parameters:  map[string]interface{}{ParamVMSCAddress: "905304445566"},
		}
	})
	c := newClient(t, nc)

	res, err := c.SendRoutingInfoForSM(context.Background(), SRIRequest{
		MSISDN:               "905321112233",
		ServiceCentreAddress: E164("905001112233"),
		HLRGlobalTitle:       "905300000099",
		HLRPointCode:         0x0102,
	})
	if err != nil {
		t.Fatalf("SRI: %v", err)
	}
	if res.IMSI != "286010000000123" || res.NetworkNodeNumber != "905304445566" || res.NodeID != "node-2" {
		t.Fatalf("result = %+v", res)
	}
}

func TestMTForwardSM_Success(t *testing.T) {
	nc := startNATS(t)
	tpdu := []byte{0x04, 0x0B, 0x91, 0x90, 0x53, 0x21, 0x11, 0x22, 0x33, 0x00}
	fakeGateway(t, nc, SubjectMTSMSRequest, SubjectMTSMSResponse, func(req *Message) *Message {
		if req.MessageType != MsgTypeMAPMTSMS || req.IMSI != "286010000000123" {
			t.Errorf("request = %s imsi=%q", req.MessageType, req.IMSI)
		}
		if string(req.PayloadData) != string(tpdu) {
			t.Errorf("payload_data = % x", req.PayloadData)
		}
		if nnn := addressParam(t, req, ParamNetworkNodeNumber); nnn["digits"] != "905304445566" {
			t.Errorf("network_node_number = %v", nnn)
		}
		return &Message{MessageType: MsgTypeMAPMTSMS, Status: StatusDelivered}
	})
	c := newClient(t, nc)

	res, err := c.MTForwardSM(context.Background(), MTRequest{
		IMSI:                 "286010000000123",
		NetworkNodeNumber:    E164("905304445566"),
		ServiceCentreAddress: E164("905001112233"),
		TPDU:                 tpdu,
	})
	if err != nil {
		t.Fatalf("MT: %v", err)
	}
	if res.Status != StatusDelivered {
		t.Fatalf("status = %q", res.Status)
	}
}

func TestGatewayFailureIsGatewayError(t *testing.T) {
	nc := startNATS(t)
	fakeGateway(t, nc, SubjectSRIRequest, SubjectSRIResponse, func(*Message) *Message {
		return &Message{
			MessageType:  MsgTypeMAPSRI,
			Status:       StatusTimeout,
			ErrorCode:    ErrCodeTimeout,
			ErrorMessage: "dialogue timeout",
			NodeID:       "node-1",
		}
	})
	c := newClient(t, nc)

	_, err := c.SendRoutingInfoForSM(context.Background(), SRIRequest{MSISDN: "1", ServiceCentreAddress: E164("2")})
	var gwErr *GatewayError
	if !errors.As(err, &gwErr) {
		t.Fatalf("err = %v, want *GatewayError", err)
	}
	if gwErr.Status != StatusTimeout || gwErr.Code != ErrCodeTimeout || gwErr.Message != "dialogue timeout" || gwErr.NodeID != "node-1" {
		t.Fatalf("GatewayError = %+v", gwErr)
	}
}

// An unknown status must never read as success.
func TestUnknownStatusIsFailure(t *testing.T) {
	nc := startNATS(t)
	fakeGateway(t, nc, SubjectMTSMSRequest, SubjectMTSMSResponse, func(*Message) *Message {
		return &Message{MessageType: MsgTypeMAPMTSMS, Status: "SOMETHING_NEW"}
	})
	c := newClient(t, nc)
	_, err := c.MTForwardSM(context.Background(), MTRequest{
		IMSI: "1", NetworkNodeNumber: E164("2"), ServiceCentreAddress: E164("3"), TPDU: []byte{1},
	})
	var gwErr *GatewayError
	if !errors.As(err, &gwErr) || gwErr.Status != "SOMETHING_NEW" {
		t.Fatalf("err = %v", err)
	}
}

// Responses arrive out of order and every client instance sees all of them;
// each caller must get exactly its own.
func TestConcurrentRequestsMatchByCorrelationID(t *testing.T) {
	nc := startNATS(t)
	const n = 50
	var (
		mu   sync.Mutex
		held []*Message
	)
	release := make(chan struct{})
	fakeGateway(t, nc, SubjectSRIRequest, SubjectSRIResponse, func(req *Message) *Message {
		mu.Lock()
		held = append(held, req)
		full := len(held) == n
		mu.Unlock()
		if full {
			close(release)
		}
		return nil
	})
	go func() {
		<-release
		mu.Lock()
		defer mu.Unlock()
		for i := len(held) - 1; i >= 0; i-- { // reverse order
			data, _ := json.Marshal(&Message{
				CorrelationID: held[i].CorrelationID,
				MessageType:   MsgTypeMAPSRI,
				IMSI:          "imsi-" + held[i].MSISDN,
			})
			_ = nc.Publish(SubjectSRIResponse, data)
		}
	}()

	a, b := newClient(t, nc), newClient(t, nc) // two instances share the response subject
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		c := a
		if i%2 == 1 {
			c = b
		}
		msisdn := fmt.Sprintf("9053200000%02d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			res, err := c.SendRoutingInfoForSM(ctx, SRIRequest{MSISDN: msisdn, ServiceCentreAddress: E164("1")})
			switch {
			case err != nil:
				errs <- err
			case res.IMSI != "imsi-"+msisdn:
				errs <- fmt.Errorf("msisdn %s got %s", msisdn, res.IMSI)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for _, c := range []*Client{a, b} {
		c.mu.Lock()
		if len(c.pending) != 0 {
			t.Errorf("pending not drained: %d", len(c.pending))
		}
		c.mu.Unlock()
	}
}

func TestDeadline(t *testing.T) {
	nc := startNATS(t)
	fakeGateway(t, nc, SubjectSRIRequest, SubjectSRIResponse, func(*Message) *Message { return nil })

	t.Run("caller deadline", func(t *testing.T) {
		c := newClient(t, nc)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := c.SendRoutingInfoForSM(ctx, SRIRequest{MSISDN: "1", ServiceCentreAddress: E164("2")})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
		c.mu.Lock()
		leaked := len(c.pending)
		c.mu.Unlock()
		if leaked != 0 {
			t.Fatalf("pending entry leaked")
		}
	})
	t.Run("default deadline", func(t *testing.T) {
		c := newClient(t, nc, WithDefaultTimeout(100*time.Millisecond))
		start := time.Now()
		_, err := c.SendRoutingInfoForSM(context.Background(), SRIRequest{MSISDN: "1", ServiceCentreAddress: E164("2")})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("default timeout not applied: %v", d)
		}
	})
}

func TestInvalidRequestIsNotPublished(t *testing.T) {
	nc := startNATS(t)
	published := make(chan struct{}, 8)
	for _, s := range []string{SubjectSRIRequest, SubjectMTSMSRequest} {
		sub, err := nc.Subscribe(s, func(*nats.Msg) { published <- struct{}{} })
		if err != nil {
			t.Fatal(err)
		}
		defer sub.Unsubscribe()
	}
	c := newClient(t, nc)
	ctx := context.Background()
	valid := MTRequest{IMSI: "1", NetworkNodeNumber: E164("2"), ServiceCentreAddress: E164("3"), TPDU: []byte{1}}

	cases := map[string]func() error{
		"SRI no MSISDN": func() error {
			_, err := c.SendRoutingInfoForSM(ctx, SRIRequest{ServiceCentreAddress: E164("1")})
			return err
		},
		"SRI no SCA": func() error {
			_, err := c.SendRoutingInfoForSM(ctx, SRIRequest{MSISDN: "1"})
			return err
		},
		"MT no IMSI": func() error { r := valid; r.IMSI = ""; _, err := c.MTForwardSM(ctx, r); return err },
		"MT no NNN": func() error {
			r := valid
			r.NetworkNodeNumber = AddressString{}
			_, err := c.MTForwardSM(ctx, r)
			return err
		},
		"MT no SCA": func() error {
			r := valid
			r.ServiceCentreAddress = AddressString{}
			_, err := c.MTForwardSM(ctx, r)
			return err
		},
		"MT no TPDU": func() error { r := valid; r.TPDU = nil; _, err := c.MTForwardSM(ctx, r); return err },
	}
	for name, call := range cases {
		if err := call(); !errors.Is(err, ErrInvalidRequest) {
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

func TestCloseFailsPendingAndLaterRequests(t *testing.T) {
	nc := startNATS(t)
	received := make(chan struct{})
	fakeGateway(t, nc, SubjectSRIRequest, SubjectSRIResponse, func(*Message) *Message {
		close(received)
		return nil
	})
	c, err := New(nc)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.SendRoutingInfoForSM(context.Background(), SRIRequest{MSISDN: "1", ServiceCentreAddress: E164("2")})
		done <- err
	}()
	<-received
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatalf("pending err = %v, want ErrClosed", err)
	}
	if _, err := c.SendRoutingInfoForSM(context.Background(), SRIRequest{MSISDN: "1", ServiceCentreAddress: E164("2")}); !errors.Is(err, ErrClosed) {
		t.Fatalf("after Close err = %v, want ErrClosed", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestSubscribeMO(t *testing.T) {
	nc := startNATS(t)
	var decodeErrs []string
	var mu sync.Mutex
	c := newClient(t, nc, WithDecodeErrorHandler(func(subject string, _ []byte, _ error) {
		mu.Lock()
		decodeErrs = append(decodeErrs, subject)
		mu.Unlock()
	}))
	got := make(chan *Message, 1)
	sub, err := c.SubscribeMO("smsc", func(m *Message) { got <- m })
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}

	if err := nc.Publish(SubjectMOSMS, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(&Message{
		MessageID: "m1", CorrelationID: "mo-1", MessageType: MsgTypeMAPMOSMS, Direction: DirectionOutbound,
		Sender: "905309990000", Recipient: "905300000000", IMSI: "286010000000999",
		PayloadData: []byte{0x01, 0x02}, NodeID: "node-3",
	})
	if err := nc.Publish(SubjectMOSMS, data); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m.CorrelationID != "mo-1" || m.Sender != "905309990000" || string(m.PayloadData) != "\x01\x02" || m.NodeID != "node-3" {
			t.Fatalf("MO = %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("MO not delivered")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(decodeErrs) != 1 || decodeErrs[0] != SubjectMOSMS {
		t.Fatalf("decode errors = %v", decodeErrs)
	}
}

// The JSON field names are the contract non-Go consumers (the Java SMSC)
// implement against. This pins them.
func TestMessageWireFieldNames(t *testing.T) {
	m := Message{
		MessageID: "a", CorrelationID: "b", MessageType: MsgTypeMAPSRI, Direction: DirectionInbound,
		Timestamp: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC),
		Sender:    "c", Recipient: "d", IMSI: "e", MSISDN: "f",
		OPC: 1, DPC: 2, SSN: 3, GlobalTitle: "g",
		Content: "h", PayloadData: []byte{1}, Parameters: map[string]interface{}{"k": "v"},
		ServiceKey: 4, EventType: "i", Status: "j", ErrorCode: -1, ErrorMessage: "k",
		TraceID: "l", NodeID: "m",
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"message_id":"a","correlation_id":"b","message_type":"MAP_SRI","direction":"INBOUND",` +
		`"timestamp":"2026-10-12T00:00:00Z","sender":"c","recipient":"d","imsi":"e","msisdn":"f",` +
		`"opc":1,"dpc":2,"ssn":3,"global_title":"g","content":"h","payload_data":"AQ==",` +
		`"parameters":{"k":"v"},"service_key":4,"event_type":"i","status":"j","error_code":-1,` +
		`"error_message":"k","trace_id":"l","node_id":"m"}`
	if string(data) != want {
		t.Fatalf("wire format changed:\n got %s\nwant %s", data, want)
	}
}

// New subscribes to nothing; a request subscribes to its own response
// subject only, so an account allowed some operations works without the
// others' subjects.
func TestSubscribesPerOperation(t *testing.T) {
	nc := startNATS(t)
	c := newClient(t, nc)
	if n := nc.NumSubscriptions(); n != 0 {
		t.Fatalf("New left %d subscriptions, want 0", n)
	}
	fakeGateway(t, nc, SubjectRoutingRequest, SubjectRoutingResponse, func(*Message) *Message {
		return &Message{MessageType: MsgTypeMAPRouting, RoutingInfo: &RoutingInfo{}}
	})
	before := nc.NumSubscriptions()
	for i := 0; i < 2; i++ {
		if _, err := c.SendRoutingInfo(context.Background(), RoutingRequest{MSISDN: "1"}); err != nil {
			t.Fatal(err)
		}
	}
	if n := nc.NumSubscriptions() - before; n != 1 {
		t.Fatalf("two routing requests added %d subscriptions, want 1", n)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if n := nc.NumSubscriptions() - before; n != 0 {
		t.Fatalf("Close left %d subscriptions", n)
	}
	if _, err := c.SendRoutingInfo(context.Background(), RoutingRequest{MSISDN: "1"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("request after Close: %v", err)
	}
}
