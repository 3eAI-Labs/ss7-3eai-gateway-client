// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: Apache-2.0

package ss7client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/nats-io/nats.go"
)

// USSD (3GPP TS 29.002 §11.9, TS 23.090), with the gateway as gsmSCF under
// networkUnstructuredSsContext-v2. Needs gateway 0.7.7 or later.
//
// A USSD exchange is a session: one TCAP dialogue that stays open while the
// subscriber reads and answers. A session starts in one of two ways:
//
//   - The subscriber starts it (dials *123#): the HLR opens a dialogue with
//     processUnstructuredSS-Request, and the gateway publishes a "begin"
//     event on the route the dialed string matches (SubscribeUSSD). The
//     application answers with USSDRequest (a prompt; returns the reply)
//     and USSDNotify (text the handset displays), and ends it with
//     USSDRelease and the final text — or USSDReject and a MAP error.
//   - The application starts it (network-initiated): StartUSSD sends
//     unstructuredSS-Request or unstructuredSS-Notify to the subscriber's
//     HLR; the session continues the same way and ends with USSDRelease
//     (no final text: a last message is a USSDNotify before it).
//
// # Endings
//
// Every session ends exactly once, and the gateway publishes one "end"
// event for it on its route, whoever ended it. Core NATS delivers at most
// once: an event (or answer) lost on the way is not sent again, so keep a
// timeout of your own for every session. A reply the gateway cannot publish
// ends its session (StatusAborted). The endings:
//
//	Status          Code                   meaning
//	""              0                      released: Detail says by whom
//	                                       ("application" or "network")
//	StatusTimeout   ErrCodeTimeout         map.ussd_session_timeout passed, or
//	                                       the network did not answer in time
//	StatusAborted   ErrCodeAborted         the dialogue was aborted (the
//	                                       subscriber cancelled, a node failed)
//	StatusRemoteError a MAP error code     the network refused a request or
//	                                       notify (absentSubscriber, ussd-Busy…)
//
// A command that fails because the session ended returns the same status as
// a *GatewayError, and the end event is published as well: the error is the
// caller's view of the ending, the event everyone's.
//
// # Routing and sessions
//
// A session lives on the gateway node that holds its dialogue. Commands go
// to every node on SubjectUSSDCommand; the node that holds the session
// acts, the others ignore the command. The holding node answers a command
// it cannot carry out with StatusRejected: an unknown or ended session, a
// request or notify while another is outstanding, USSDRelease without the
// final text of a subscriber-initiated session (or with one for a
// network-initiated session), USSDReject on a network-initiated session.
// USSDRelease and USSDReject are carried out with a request or notify
// outstanding: that one fails with StatusAborted. Only if that node is gone
// does a command go unanswered, and the caller's deadline applies.
//
// A session waits for one thing at a time — the subscriber's answer to the
// request or notify outstanding, else the application's next command — and
// for at most map.ussd_session_timeout each time (the operations' timer ml,
// TS 29.002 §17.1.2: 1 to 10 minutes).
//
// Session IDs are opaque strings of at most 64 characters, unguessable;
// do not parse them.
const (
	// SubjectUSSDEventPrefix is the prefix of the event subjects, one per
	// route: map.ussd.event.<route>.
	SubjectUSSDEventPrefix = "map.ussd.event."
	// SubjectUSSDStart carries network-initiated session starts,
	// application → gateway (queue group).
	SubjectUSSDStart = "map.ussd.start"
	// SubjectUSSDCommand carries commands on an existing session,
	// application → every gateway node.
	SubjectUSSDCommand = "map.ussd.command"
	// SubjectUSSDResponse carries the answers to starts and commands,
	// gateway → application.
	SubjectUSSDResponse = "map.ussd.response"

	// USSDDefaultRoute receives the subscriber-initiated sessions no
	// configured route matches (map.ussd_routes), and the network-initiated
	// ones started without a route.
	USSDDefaultRoute = "default"
)

const MsgTypeMAPUSSD MessageType = "MAP_USSD"

// Values of USSD.Action.
const (
	// Events.
	USSDBegin = "begin" // a subscriber started a session; USSD carries what they sent
	USSDEnd   = "end"   // the session is over (see Endings)

	// Commands.
	USSDRequest = "request" // show text and collect the subscriber's reply
	USSDNotify  = "notify"  // show text; the handset confirms receipt
	USSDRelease = "release" // end the session; with the final text of a subscriber-initiated one
	USSDReject  = "reject"  // end a subscriber-initiated session with USSD.Error
)

// Data coding schemes the gateway uses when it encodes Text (TS 23.038 §5).
const (
	DCSGSM7 byte = 0x0F // GSM 7 bit default alphabet, language unspecified
	DCSUCS2 byte = 0x48 // general data coding, uncompressed, no class, UCS2
)

// MaxUSSDOctets is maxUSSD-StringLength (TS 29.002): at most 182 GSM 7-bit
// characters (two for each of the extension table's: €, [, ], …) or 80
// UCS2 characters.
const MaxUSSDOctets = 160

// DefaultUSSDTimeout bounds a USSD call whose context has no deadline. A
// human is reading and typing; the gateway's own session timeout
// (map.ussd_session_timeout, 180 s by default) ends the session regardless.
const DefaultUSSDTimeout = 3*time.Minute + 10*time.Second

// MAP error codes of the USSD operations (TS 29.002 MAP-ErrorCodes) not
// already defined. USSDReject answers with one of systemFailure, dataMissing,
// unexpectedDataValue, unknownAlphabet and callBarred; the network answers a
// request or notify with these and absentSubscriber, illegalSubscriber,
// illegalEquipment, ussd-Busy.
const (
	MAPErrUnknownAlphabet int32 = 71
	MAPErrUSSDBusy        int32 = 72
)

// The errors processUnstructuredSS-Request allows (TS 29.002 §11.9.1).
var ussdRejectCodes = map[int32]bool{
	MAPErrSystemFailure: true, MAPErrDataMissing: true, MAPErrUnexpectedDataValue: true,
	MAPErrUnknownAlphabet: true, MAPErrCallBarred: true,
}

var routeName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// USSD is the USSD part of a Message on the USSD subjects.
type USSD struct {
	SessionID string `json:"session_id,omitempty"`
	Action    string `json:"action"`
	// Route is the route a session belongs to (events, and a start that
	// names one).
	Route string `json:"route,omitempty"`
	// DCS and Data are the USSD data coding scheme (TS 23.038 §5) and the
	// USSD-String (base64 in JSON). An answer without them is an empty
	// reply, which unstructuredSS-Request allows.
	DCS  *byte  `json:"dcs,omitempty"`
	Data []byte `json:"data,omitempty"`
	// Text is Data decoded, when the DCS is GSM 7-bit or UCS2. A language
	// indication the DCS announces (0x10, 0x11) stays at its start. In a
	// command, Text alone asks the gateway to encode it (GSM 7-bit when
	// every character is in the default alphabet, else UCS2).
	Text string `json:"text,omitempty"`
	// Error is the MAP error of a reject command.
	Error int32 `json:"error,omitempty"`
	// Detail says who released a session, in an end event; in a begin, why
	// Text is missing when the string could not be decoded.
	Detail string `json:"detail,omitempty"`
}

// USSDText is what an application sends: Text for the gateway to encode,
// or DCS and Data as they are to go out. The gateway's length check, on the
// encoded octets, is authoritative.
type USSDText struct {
	Text string
	DCS  *byte
	Data []byte
}

func (t USSDText) wire(op string) (*USSD, error) {
	u := &USSD{}
	switch {
	case t.Text != "" && t.Data == nil && t.DCS == nil:
		// A loose bound: GSM 7-bit is at most 182 characters; the gateway
		// counts the encoded octets (UCS2: 80 characters).
		if n := utf8.RuneCountInString(t.Text); n > 182 {
			return nil, fmt.Errorf("%w: %s: text of %d characters is over any USSD length", ErrInvalidRequest, op, n)
		}
		u.Text = t.Text
	case t.Text == "" && t.Data != nil && t.DCS != nil:
		if len(t.Data) == 0 || len(t.Data) > MaxUSSDOctets {
			return nil, fmt.Errorf("%w: %s: data of %d octets, want 1..%d", ErrInvalidRequest, op, len(t.Data), MaxUSSDOctets)
		}
		dcs := *t.DCS
		u.DCS, u.Data = &dcs, t.Data
	default:
		return nil, fmt.Errorf("%w: %s: Text, or DCS and Data", ErrInvalidRequest, op)
	}
	return u, nil
}

// USSDMessage is what the subscriber sent.
type USSDMessage struct {
	// Empty: the reply carried no USSD string.
	Empty bool
	DCS   byte
	Data  []byte
	// Text is Data decoded; empty when the DCS is not GSM 7-bit or UCS2.
	Text string
}

// USSDEvent is a session begin or end.
type USSDEvent struct {
	SessionID string
	Action    string // USSDBegin or USSDEnd
	Route     string
	// Begin: the subscriber — the IMSI or MSISDN of the HLR's destination
	// reference, the MSISDN of its argument; either may be missing — and
	// what they sent.
	MSISDN, IMSI string
	Message      *USSDMessage
	// End: how (see Endings).
	Status string
	Code   int32
	Detail string
	NodeID string
	Raw    *Message
}

// USSDStart is a network-initiated session.
type USSDStart struct {
	// Exactly one of MSISDN and IMSI (digits) identifies the subscriber. The
	// HLR is addressed by the MSISDN as global title, or by map.hlr_gt /
	// map.hlr_pc (map.sri_routing "hlr"); a start by IMSI needs the latter.
	MSISDN, IMSI string
	// Notify sends unstructuredSS-Notify instead of unstructuredSS-Request.
	Notify  bool
	Message USSDText
	// Route receives the session's end event; USSDDefaultRoute when empty.
	Route string
}

// USSDAnswer is the outcome of a request or notify: the session, and for a
// request what the subscriber replied (possibly Empty).
type USSDAnswer struct {
	SessionID string
	Reply     *USSDMessage // nil for a notify
	NodeID    string
	Raw       *Message
}

var errNoUSSD = errors.New("ss7 client: USSD response without ussd")

// ussdContext applies DefaultUSSDTimeout (or WithUSSDTimeout) when ctx has
// no deadline. After a deadline passes the session may still be live: end
// it with USSDRelease or USSDReject on a fresh context, or the gateway ends
// it at its session timeout.
func (c *Client) ussdContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.ussdTimeout)
}

// StartUSSD starts a network-initiated session: unstructuredSS-Request
// (answered with the subscriber's reply) or, with Notify,
// unstructuredSS-Notify (answered when the handset has displayed it), to
// the subscriber's HLR. The session stays open: continue it with
// USSDRequest or USSDNotify, end it with USSDRelease.
func (c *Client) StartUSSD(ctx context.Context, req USSDStart) (*USSDAnswer, error) {
	if (req.MSISDN == "") == (req.IMSI == "") {
		return nil, fmt.Errorf("%w: USSD: exactly one of MSISDN and IMSI required", ErrInvalidRequest)
	}
	if err := checkDigits("USSD", map[string]string{"MSISDN": req.MSISDN, "IMSI": req.IMSI}); err != nil {
		return nil, err
	}
	if err := checkISDNLength("USSD", "MSISDN", req.MSISDN); err != nil {
		return nil, err
	}
	if len(req.IMSI) > 15 {
		return nil, fmt.Errorf("%w: USSD: IMSI of %d digits, at most 15", ErrInvalidRequest, len(req.IMSI))
	}
	if req.Route != "" && !routeName.MatchString(req.Route) {
		return nil, fmt.Errorf("%w: USSD: route %q is not 1..32 of [A-Za-z0-9_-]", ErrInvalidRequest, req.Route)
	}
	u, err := req.Message.wire("USSD")
	if err != nil {
		return nil, err
	}
	u.Action, u.Route = USSDRequest, req.Route
	if req.Notify {
		u.Action = USSDNotify
	}
	ctx, cancel := c.ussdContext(ctx)
	defer cancel()
	resp, err := c.request(ctx, SubjectUSSDStart, &Message{
		MessageType: MsgTypeMAPUSSD, MSISDN: req.MSISDN, IMSI: req.IMSI, USSD: u,
	})
	if err != nil {
		return nil, err
	}
	return ussdAnswer(resp, !req.Notify)
}

// USSDRequest shows text to the subscriber of a session and returns their
// reply.
func (c *Client) USSDRequest(ctx context.Context, sessionID string, t USSDText) (*USSDAnswer, error) {
	resp, err := c.ussdCommand(ctx, sessionID, USSDRequest, &t, 0)
	if err != nil {
		return nil, err
	}
	return ussdAnswer(resp, true)
}

// USSDNotify shows text to the subscriber of a session and returns once the
// handset has displayed it.
func (c *Client) USSDNotify(ctx context.Context, sessionID string, t USSDText) (*USSDAnswer, error) {
	resp, err := c.ussdCommand(ctx, sessionID, USSDNotify, &t, 0)
	if err != nil {
		return nil, err
	}
	return ussdAnswer(resp, false)
}

// USSDRelease ends a session. A session the subscriber started needs the
// final text they are shown (processUnstructuredSS-Request's result); for a
// network-initiated one final must be nil.
func (c *Client) USSDRelease(ctx context.Context, sessionID string, final *USSDText) error {
	_, err := c.ussdCommand(ctx, sessionID, USSDRelease, final, 0)
	return err
}

// USSDReject ends a session the subscriber started with a MAP error instead
// of a final text: MAPErrSystemFailure, MAPErrDataMissing,
// MAPErrUnexpectedDataValue, MAPErrUnknownAlphabet or MAPErrCallBarred.
func (c *Client) USSDReject(ctx context.Context, sessionID string, mapError int32) error {
	if !ussdRejectCodes[mapError] {
		return fmt.Errorf("%w: USSD: MAP error %d is not one processUnstructuredSS-Request allows", ErrInvalidRequest, mapError)
	}
	_, err := c.ussdCommand(ctx, sessionID, USSDReject, nil, mapError)
	return err
}

func (c *Client) ussdCommand(ctx context.Context, sessionID, action string, t *USSDText, mapError int32) (*Message, error) {
	if sessionID == "" || len(sessionID) > 64 {
		return nil, fmt.Errorf("%w: USSD %s: session ID of %d characters, want 1..64", ErrInvalidRequest, action, len(sessionID))
	}
	u := &USSD{}
	if t != nil {
		var err error
		if u, err = t.wire("USSD " + action); err != nil {
			return nil, err
		}
	}
	u.SessionID, u.Action, u.Error = sessionID, action, mapError
	ctx, cancel := c.ussdContext(ctx)
	defer cancel()
	return c.request(ctx, SubjectUSSDCommand, &Message{MessageType: MsgTypeMAPUSSD, USSD: u})
}

func ussdAnswer(resp *Message, wantReply bool) (*USSDAnswer, error) {
	if resp.USSD == nil || resp.USSD.SessionID == "" {
		return nil, errNoUSSD
	}
	a := &USSDAnswer{SessionID: resp.USSD.SessionID, NodeID: resp.NodeID, Raw: resp}
	if wantReply {
		m, err := resp.USSD.message()
		if err != nil {
			return nil, err
		}
		a.Reply = m
	}
	return a, nil
}

// message is what the gateway reports the subscriber sent: an empty reply
// when it carries neither DCS nor data.
func (u *USSD) message() (*USSDMessage, error) {
	switch {
	case u.DCS == nil && len(u.Data) == 0:
		return &USSDMessage{Empty: true}, nil
	case u.DCS == nil || len(u.Data) == 0:
		return nil, errors.New("ss7 client: USSD message with only one of dcs and data")
	}
	return &USSDMessage{DCS: *u.DCS, Data: u.Data, Text: u.Text}, nil
}

// SubscribeUSSD delivers the begins and ends of a route's sessions
// (map.ussd.event.<route>; USSDDefaultRoute for sessions no configured route
// matches). Use a queue group: each event then goes to one member of it,
// and the member that sees a begin need not be the one that sees its end —
// keep session state where every member can reach it, by session ID. An
// empty queue gives every subscriber every event, which also lets two
// instances answer the same session.
//
// handle runs on the subscription's single goroutine: it must not wait for a
// subscriber's answer — run each session in a goroutine of its own. A begin
// is delivered even when what the subscriber sent cannot be read: with
// Message nil, or with Message.Text empty and Detail saying why (the DCS
// names an alphabet the gateway does not decode — 8-bit data, compressed —
// leaves Text empty without a Detail). Reject what you cannot serve
// (MAPErrUnexpectedDataValue, MAPErrUnknownAlphabet).
func (c *Client) SubscribeUSSD(route, queue string, handle func(*USSDEvent)) (*nats.Subscription, error) {
	if handle == nil {
		return nil, errors.New("ss7 client: nil USSD handler")
	}
	if !routeName.MatchString(route) {
		return nil, fmt.Errorf("%w: route %q is not 1..32 of [A-Za-z0-9_-]", ErrInvalidRequest, route)
	}
	subject := SubjectUSSDEventPrefix + route
	cb := func(m *nats.Msg) {
		var msg Message
		if err := json.Unmarshal(m.Data, &msg); err != nil || msg.USSD == nil {
			if c.onDecodeError != nil {
				if err == nil {
					err = errNoUSSD
				}
				c.onDecodeError(m.Subject, m.Data, err)
			}
			return
		}
		ev := &USSDEvent{
			SessionID: msg.USSD.SessionID, Action: msg.USSD.Action, Route: msg.USSD.Route,
			MSISDN: msg.MSISDN, IMSI: msg.IMSI,
			Status: msg.Status, Code: msg.ErrorCode, Detail: msg.ErrorMessage,
			NodeID: msg.NodeID, Raw: &msg,
		}
		if ev.Detail == "" {
			ev.Detail = msg.USSD.Detail
		}
		if msg.USSD.Action == USSDBegin {
			var err error
			if ev.Message, err = msg.USSD.message(); err != nil {
				ev.Detail = err.Error()
			}
		}
		handle(ev)
	}
	if queue == "" {
		return c.nc.Subscribe(subject, cb)
	}
	return c.nc.QueueSubscribe(subject, queue, cb)
}

// WithUSSDTimeout replaces DefaultUSSDTimeout for USSD calls without a
// deadline.
func WithUSSDTimeout(d time.Duration) Option {
	return func(c *Client) { c.ussdTimeout = d }
}
