// Copyright 2026 3eAI Labs Ltd
// SPDX-License-Identifier: Apache-2.0

package ss7client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// DefaultTimeout bounds a request whose context has no deadline. It is longer
// than the gateway's default TCAP dialogue timeout (tcap.dialogue_timeout,
// 30s), so the gateway's own TIMEOUT response normally arrives first.
const DefaultTimeout = 40 * time.Second

var (
	// ErrInvalidRequest wraps a request rejected before publishing because
	// a field the gateway requires is missing.
	ErrInvalidRequest = errors.New("ss7 client: invalid request")
	// ErrClosed is returned for requests made after, or pending at, Close.
	ErrClosed = errors.New("ss7 client: closed")
)

// GatewayError is a failure the gateway reported on a response subject.
type GatewayError struct {
	CorrelationID string
	Status        string // StatusTimeout, StatusAborted, ...
	Code          int32  // ErrCode* (negative) or a MAP error code (positive, MAPErr*)
	Message       string
	NodeID        string
}

func (e *GatewayError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("ss7 gateway: %s (code %d): %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("ss7 gateway: %s (code %d)", e.Status, e.Code)
}

// Option configures a Client.
type Option func(*Client)

// WithDefaultTimeout replaces DefaultTimeout for requests without a deadline.
func WithDefaultTimeout(d time.Duration) Option {
	return func(c *Client) { c.defaultTimeout = d }
}

// WithDecodeErrorHandler receives messages on the client's subscriptions that
// are not valid JSON Messages. Without it they are dropped.
func WithDecodeErrorHandler(f func(subject string, data []byte, err error)) Option {
	return func(c *Client) { c.onDecodeError = f }
}

// Client sends MAP requests to the gateway and matches the responses by
// correlation ID. It is safe for concurrent use. It does not own the NATS
// connection: Close releases the client's subscriptions only.
type Client struct {
	nc             *nats.Conn
	defaultTimeout time.Duration
	onDecodeError  func(subject string, data []byte, err error)

	subs []*nats.Subscription

	mu      sync.Mutex
	closed  bool
	pending map[string]chan *Message
}

// New subscribes to the response subjects and returns a ready Client.
func New(nc *nats.Conn, opts ...Option) (*Client, error) {
	if nc == nil {
		return nil, errors.New("ss7 client: nil NATS connection")
	}
	c := &Client{
		nc:             nc,
		defaultTimeout: DefaultTimeout,
		pending:        make(map[string]chan *Message),
	}
	for _, o := range opts {
		o(c)
	}
	for _, subject := range []string{SubjectSRIResponse, SubjectMTSMSResponse, SubjectATIResponse, SubjectPSIResponse} {
		sub, err := nc.Subscribe(subject, c.onResponse)
		if err != nil {
			c.unsubscribe()
			return nil, fmt.Errorf("ss7 client: subscribe %s: %w", subject, err)
		}
		c.subs = append(c.subs, sub)
	}
	// The server must have the subscriptions before the first request goes
	// out, or a fast response could pass us by.
	if err := nc.Flush(); err != nil {
		c.unsubscribe()
		return nil, fmt.Errorf("ss7 client: flush: %w", err)
	}
	return c, nil
}

// Close unsubscribes and fails every pending request with ErrClosed.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	return c.unsubscribe()
}

func (c *Client) unsubscribe() error {
	var errs []error
	for _, s := range c.subs {
		if err := s.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
			errs = append(errs, err)
		}
	}
	c.subs = nil
	return errors.Join(errs...)
}

func (c *Client) onResponse(m *nats.Msg) {
	var msg Message
	if err := json.Unmarshal(m.Data, &msg); err != nil {
		if c.onDecodeError != nil {
			c.onDecodeError(m.Subject, m.Data, err)
		}
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[msg.CorrelationID]
	if ok {
		delete(c.pending, msg.CorrelationID)
	}
	c.mu.Unlock()
	if ok {
		ch <- &msg // buffered(1), and removed from pending above: never blocks
	}
}

// request publishes msg on subject and waits for the response carrying its
// correlation ID.
func (c *Client) request(ctx context.Context, subject string, msg *Message) (*Message, error) {
	if _, ok := ctx.Deadline(); !ok && c.defaultTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.defaultTimeout)
		defer cancel()
	}

	msg.MessageID = uuid.NewString()
	msg.CorrelationID = uuid.NewString()
	msg.Direction = DirectionInbound
	msg.Timestamp = time.Now().UTC()
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("ss7 client: encode: %w", err)
	}

	ch := make(chan *Message, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.pending[msg.CorrelationID] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, msg.CorrelationID)
		c.mu.Unlock()
	}()

	if err := c.nc.Publish(subject, data); err != nil {
		return nil, fmt.Errorf("ss7 client: publish %s: %w", subject, err)
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, ErrClosed
		}
		if isFailure(resp) {
			return resp, &GatewayError{
				CorrelationID: resp.CorrelationID,
				Status:        resp.Status,
				Code:          resp.ErrorCode,
				Message:       resp.ErrorMessage,
				NodeID:        resp.NodeID,
			}
		}
		return resp, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("ss7 client: %s (correlation_id %s): %w", subject, msg.CorrelationID, ctx.Err())
	}
}

// isFailure treats any status other than empty (success of an interrogation:
// SRI, ATI, PSI) or DELIVERED (MT success) as a failure, so a status this client does not know yet is
// never reported as success.
func isFailure(m *Message) bool {
	if m.ErrorCode != 0 || m.ErrorMessage != "" {
		return true
	}
	return m.Status != "" && m.Status != StatusDelivered
}

// SRIRequest is a MAP SendRoutingInfoForSM request (3GPP TS 29.002 §12.1).
type SRIRequest struct {
	MSISDN               string        // required, E.164 digits
	ServiceCentreAddress AddressString // required
	// HLRGlobalTitle and HLRPointCode override the gateway's map.hlr_gt and
	// map.hlr_pc for this request. Leave empty to use the configuration.
	HLRGlobalTitle string
	HLRPointCode   uint32
}

// SRIResult is a successful SendRoutingInfoForSM answer.
type SRIResult struct {
	IMSI string
	// NetworkNodeNumber is the serving MSC/VLR address digits; pass it to
	// MTRequest.NetworkNodeNumber.
	NetworkNodeNumber string
	NodeID            string // gateway node that ran the dialogue
	Raw               *Message
}

// SendRoutingInfoForSM asks the subscriber's HLR where to deliver a short
// message. On a gateway-reported failure it returns *GatewayError.
func (c *Client) SendRoutingInfoForSM(ctx context.Context, req SRIRequest) (*SRIResult, error) {
	if req.MSISDN == "" {
		return nil, fmt.Errorf("%w: SRI: MSISDN required", ErrInvalidRequest)
	}
	if req.ServiceCentreAddress.Digits == "" {
		return nil, fmt.Errorf("%w: SRI: ServiceCentreAddress required", ErrInvalidRequest)
	}
	resp, err := c.request(ctx, SubjectSRIRequest, &Message{
		MessageType: MsgTypeMAPSRI,
		MSISDN:      req.MSISDN,
		GlobalTitle: req.HLRGlobalTitle,
		DPC:         req.HLRPointCode,
		Parameters: map[string]interface{}{
			ParamServiceCentreAddress: req.ServiceCentreAddress,
		},
	})
	if err != nil {
		return nil, err
	}
	nnn, _ := resp.Parameters[ParamVMSCAddress].(string)
	return &SRIResult{IMSI: resp.IMSI, NetworkNodeNumber: nnn, NodeID: resp.NodeID, Raw: resp}, nil
}

// MTRequest is a MAP MT-ForwardSM request (3GPP TS 29.002 §12.9).
type MTRequest struct {
	IMSI                 string        // required, from SRIResult.IMSI
	NetworkNodeNumber    AddressString // required, from SRIResult.NetworkNodeNumber
	ServiceCentreAddress AddressString // required
	TPDU                 []byte        // required, SMS-DELIVER TPDU (3GPP TS 23.040), opaque to the gateway
	// MSCPointCode overrides the destination point code. Zero routes on the
	// network node number GT alone.
	MSCPointCode uint32
}

// MTResult is a successful MT-ForwardSM answer.
type MTResult struct {
	Status string // StatusDelivered
	NodeID string
	Raw    *Message
}

// MTForwardSM delivers a short message to the serving MSC. On a
// gateway-reported failure it returns *GatewayError.
func (c *Client) MTForwardSM(ctx context.Context, req MTRequest) (*MTResult, error) {
	switch {
	case req.IMSI == "":
		return nil, fmt.Errorf("%w: MT: IMSI required", ErrInvalidRequest)
	case req.NetworkNodeNumber.Digits == "":
		return nil, fmt.Errorf("%w: MT: NetworkNodeNumber required", ErrInvalidRequest)
	case req.ServiceCentreAddress.Digits == "":
		return nil, fmt.Errorf("%w: MT: ServiceCentreAddress required", ErrInvalidRequest)
	case len(req.TPDU) == 0:
		return nil, fmt.Errorf("%w: MT: TPDU required", ErrInvalidRequest)
	}
	resp, err := c.request(ctx, SubjectMTSMSRequest, &Message{
		MessageType: MsgTypeMAPMTSMS,
		IMSI:        req.IMSI,
		DPC:         req.MSCPointCode,
		PayloadData: req.TPDU,
		Parameters: map[string]interface{}{
			ParamServiceCentreAddress: req.ServiceCentreAddress,
			ParamNetworkNodeNumber:    req.NetworkNodeNumber,
		},
	})
	if err != nil {
		return nil, err
	}
	return &MTResult{Status: resp.Status, NodeID: resp.NodeID, Raw: resp}, nil
}

// SubscribeMO delivers MO-Forward-SM messages the gateway received from
// foreign MSCs: Sender is the originating MSC address, Recipient the SMSC
// address, IMSI the originator when the network supplied it, PayloadData the
// SMS-SUBMIT TPDU. With a non-empty queue, instances sharing the queue name
// split the stream; with an empty queue every instance receives every
// message. The returned subscription is the caller's to unsubscribe.
//
// MO is a plain NATS publish: a message published while no subscriber is
// connected is lost.
func (c *Client) SubscribeMO(queue string, handle func(*Message)) (*nats.Subscription, error) {
	if handle == nil {
		return nil, errors.New("ss7 client: nil MO handler")
	}
	cb := func(m *nats.Msg) {
		var msg Message
		if err := json.Unmarshal(m.Data, &msg); err != nil {
			if c.onDecodeError != nil {
				c.onDecodeError(m.Subject, m.Data, err)
			}
			return
		}
		handle(&msg)
	}
	if queue == "" {
		return c.nc.Subscribe(SubjectMOSMS, cb)
	}
	return c.nc.QueueSubscribe(SubjectMOSMS, queue, cb)
}
