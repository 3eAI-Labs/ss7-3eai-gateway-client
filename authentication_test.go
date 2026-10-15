// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: Apache-2.0

package ss7client

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// gatewayRequest sends a request the way the gateway does: a NATS request
// on subject.
func gatewayRequest(t *testing.T, nc *nats.Conn, subject string, msg *Message) *Message {
	t.Helper()
	data, _ := json.Marshal(msg)
	m, err := nc.Request(subject, data, 2*time.Second)
	if err != nil {
		t.Fatalf("%s: %v", subject, err)
	}
	var reply Message
	if err := json.Unmarshal(m.Data, &reply); err != nil {
		t.Fatal(err)
	}
	return &reply
}

func hexOf(b byte, n int) string {
	return strings.Repeat(string("0123456789abcdef"[b>>4])+string("0123456789abcdef"[b&15]), n)
}

func quintupletOf(b byte) AuthenticationQuintuplet {
	return AuthenticationQuintuplet{RAND: hexOf(b, 16), XRES: hexOf(b, 8), CK: hexOf(b, 16), IK: hexOf(b, 16), AUTN: hexOf(b, 16)}
}

func TestServeAuthentication(t *testing.T) {
	nc := startNATS(t)
	var mu sync.Mutex
	var serveErrs []error
	c := newClient(t, nc, WithDecodeErrorHandler(func(_ string, _ []byte, err error) {
		mu.Lock()
		defer mu.Unlock()
		serveErrs = append(serveErrs, err)
	}))
	answers := map[string]AuthenticationAnswer{
		"vectors":       {Quintuplets: []AuthenticationQuintuplet{quintupletOf(1), quintupletOf(2)}, UEUsageType: "00000080"},
		"empty":         {},
		"refused":       {Reject: MAPErrUnknownSubscriber, RejectCause: CauseGPRSEPSSubscriptionUnknown},
		"wrong cause":   {Reject: MAPErrUnknownSubscriber, RejectCause: CauseOperatorDeterminedBarring},
		"lone cause":    {RejectCause: CauseIMSIUnknown},
		"too many":      {Quintuplets: []AuthenticationQuintuplet{quintupletOf(1), quintupletOf(2), quintupletOf(3)}},
		"both kinds":    {Quintuplets: []AuthenticationQuintuplet{quintupletOf(1)}, Triplets: []AuthenticationTriplet{{RAND: hexOf(1, 16), SRES: hexOf(1, 4), Kc: hexOf(1, 8)}}},
		"short kc":      {Triplets: []AuthenticationTriplet{{RAND: hexOf(1, 16), SRES: hexOf(1, 4), Kc: hexOf(1, 7)}}},
		"bad error":     {Reject: MAPErrRoamingNotAllowed},
		"unasked usage": {UEUsageType: "00000080"},
	}
	requests := make(chan *AuthenticationRequest, 16)
	sub, err := c.ServeAuthentication("auc", func(ctx context.Context, r *AuthenticationRequest) AuthenticationAnswer {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the handler's context has no deadline")
		}
		requests <- r
		return answers[r.DialogueID]
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	by := time.Now().Add(time.Second)
	ask := func(id string, usage bool) *Authentication {
		reply := gatewayRequest(t, nc, SubjectAuthentication, &Message{CorrelationID: "c-" + id, IMSI: "286010000000123", NodeID: "n1",
			Authentication: &Authentication{DialogueID: id, AnswerBy: &by, RequestedVectors: 2, RequestingNodeType: NodeVLR,
				UEUsageTypeRequested: usage, Resynchronisation: &Resynchronisation{RAND: hexOf(0, 16), AUTS: hexOf(1, 14)}}})
		if reply.CorrelationID != "c-"+id || reply.MessageType != MsgTypeMAPSendAuthenticationInfo || reply.Authentication.DialogueID != id {
			t.Fatalf("%s: reply envelope %+v", id, reply)
		}
		return reply.Authentication
	}

	a := ask("vectors", true)
	r := <-requests
	if r.IMSI != "286010000000123" || r.NodeID != "n1" || r.Request.Resynchronisation.AUTS != hexOf(1, 14) {
		t.Fatalf("request %+v", r)
	}
	if a.Error != 0 || len(a.Quintuplets) != 2 || a.Quintuplets[1].CK != hexOf(2, 16) || a.UEUsageType != "00000080" {
		t.Fatalf("vectors answer %+v", a)
	}
	if a = ask("empty", false); a.Error != 0 || len(a.Triplets)+len(a.Quintuplets)+len(a.EPSVectors) != 0 {
		t.Fatalf("empty answer %+v", a)
	}
	if a = ask("refused", false); a.Error != MAPErrUnknownSubscriber || a.ErrorCause != CauseGPRSEPSSubscriptionUnknown {
		t.Fatalf("refused answer %+v", a)
	}
	for _, id := range []string{"too many", "both kinds", "short kc", "bad error", "unasked usage", "wrong cause", "lone cause"} {
		if a = ask(id, false); a.Error != MAPErrSystemFailure || a.Detail == "" || len(a.Quintuplets)+len(a.Triplets) != 0 {
			t.Fatalf("%s: answer %+v, want systemFailure with the reason", id, a)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(serveErrs) != 7 {
		t.Fatalf("serve errors %v, want one per bad answer", serveErrs)
	}
}

func TestServePurges(t *testing.T) {
	nc := startNATS(t)
	c := newClient(t, nc)
	sub, err := c.ServePurges("hlr", func(_ context.Context, r *PurgeRequest) PurgeAnswer {
		switch r.IMSI {
		case "286010000000123":
			return PurgeAnswer{FreezeTMSI: r.Request.VLRNumber != nil}
		case "286010000000999":
			return PurgeAnswer{Reject: MAPErrUnknownSubscriber, RejectCause: CauseIMSIUnknown}
		case "286010000000998":
			return PurgeAnswer{Reject: MAPErrUnknownSubscriber, RejectCause: CausePLMNRoamingNotAllowed}
		}
		return PurgeAnswer{Reject: MAPErrSystemFailure} // not purgeMS's
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	vlr := E164("905321000002")
	purge := func(imsi string) *Purge {
		reply := gatewayRequest(t, nc, SubjectPurge, &Message{IMSI: imsi, Purge: &Purge{DialogueID: "d", VLRNumber: &vlr}})
		if reply.MessageType != MsgTypeMAPPurgeMS {
			t.Fatalf("reply type %s", reply.MessageType)
		}
		return reply.Purge
	}
	if p := purge("286010000000123"); !p.FreezeTMSI || p.Error != 0 {
		t.Fatalf("purge %+v", p)
	}
	if p := purge("286010000000999"); p.Error != MAPErrUnknownSubscriber || p.ErrorCause != CauseIMSIUnknown || p.FreezeTMSI {
		t.Fatalf("purge %+v", p)
	}
	if p := purge("286010000000998"); p.Error != MAPErrSystemFailure || p.ErrorCause != "" || p.Detail == "" {
		t.Fatalf("purge %+v: a cause that does not qualify the error is refused", p)
	}
	if p := purge("286010000000555"); p.Error != MAPErrSystemFailure || p.Detail == "" {
		t.Fatalf("purge %+v: an error purgeMS does not have goes as systemFailure, which the gateway aborts on", p)
	}
}

// A message on the subject without the request's part is reported, not
// served.
func TestServeAuthentication_NotARequest(t *testing.T) {
	nc := startNATS(t)
	errs := make(chan error, 1)
	c := newClient(t, nc, WithDecodeErrorHandler(func(_ string, _ []byte, err error) { errs <- err }))
	sub, err := c.ServeAuthentication("", func(context.Context, *AuthenticationRequest) AuthenticationAnswer {
		t.Error("served a message without authentication")
		return AuthenticationAnswer{}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	data, _ := json.Marshal(&Message{IMSI: "286010000000123"})
	if err := nc.Publish(SubjectAuthentication, data); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		if !strings.Contains(err.Error(), "no authentication") {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal(errors.New("not reported"))
	}
}

func TestAuthenticationVectorLimits(t *testing.T) {
	for _, tc := range []struct {
		req      Authentication
		set, eps int
	}{
		{Authentication{RequestedVectors: 3}, 3, 0},
		{Authentication{RequestedVectors: 3, RequestingNodeType: NodeSGSN, AdditionalVectors: 2}, 3, 0},
		{Authentication{RequestedVectors: 2, RequestingNodeType: NodeMME}, 0, 2},
		{Authentication{RequestedVectors: 2, RequestingNodeType: NodeMMESGSN, AdditionalVectors: 4, AdditionalVectorsAreForEPS: true}, 2, 4},
		{Authentication{RequestedVectors: 2, RequestingNodeType: NodeMMESGSN, AdditionalVectors: 4}, 4, 2},
	} {
		if set, eps := tc.req.VectorLimits(); set != tc.set || eps != tc.eps {
			t.Errorf("%+v: limits %d/%d, want %d/%d", tc.req, set, eps, tc.set, tc.eps)
		}
	}
}
