# ss7-3eai-gateway-client

Go client and NATS wire contract for [ss7-3eai-gateway](https://github.com/3eAI-Labs/ss7-3eai-gateway),
an SS7 signalling gateway (M3UA / SCCP / TCAP / MAP) that applications drive over NATS.

Licensed under the Apache License 2.0, separately from the gateway (AGPL-3.0-only), so an SMSC or
any other application can link it under any licence.

```bash
go get github.com/3eAI-Labs/ss7-3eai-gateway-client
```

## What it covers

The interfaces the gateway implements end to end — the MAP short-message path, subscriber
location and state, call routing, USSD, and location management with the gateway as HLR:

| Operation | Method | Subjects |
|-----------|--------|----------|
| SendRoutingInfoForSM (MAP op 45) | `SendRoutingInfoForSM` | `map.sri.request` → `map.sri.response` |
| MT-ForwardSM (MAP op 44) | `MTForwardSM` | `map.mt.sms.request` → `map.mt.sms.response` |
| MO-ForwardSM (MAP op 46), received from a foreign MSC | `SubscribeMO` | `map.mo.sms` |
| AnyTimeInterrogation (MAP op 71), towards the HLR | `AnyTimeInterrogation` | `map.ati.request` → `map.ati.response` |
| ProvideSubscriberInfo (MAP op 70), towards the VLR | `ProvideSubscriberInfo` | `map.psi.request` → `map.psi.response` |
| SendRoutingInfo (MAP op 22), towards the HLR, as GMSC — gateway 0.7.6 and later | `SendRoutingInfo` | `map.routing.request` → `map.routing.response` |
| USSD sessions (MAP ops 59, 60, 61), as gsmSCF — gateway 0.7.7 and later | `SubscribeUSSD`, `StartUSSD`, `USSDRequest`, `USSDNotify`, `USSDRelease`, `USSDReject` | events on `map.ussd.event.<route>`; `map.ussd.start` / `map.ussd.command` → `map.ussd.response` |
| Location updates (MAP ops 2 and 7), as HLR: the application decides — gateway 0.7.8 and later | `ServeLocationUpdates` | `map.ul` (NATS requests, gateway → application) |
| CancelLocation (MAP op 3), towards a VLR — gateway 0.7.8 and later | `CancelLocation` | `map.cl.request` → `map.cl.response` |
| ProvideRoamingNumber (MAP op 4), towards a VLR — gateway 0.7.8 and later | `ProvideRoamingNumber` | `map.prn.request` → `map.prn.response` |
| InsertSubscriberData (MAP op 7) alone, towards a VLR — gateway 0.7.8 and later | `InsertSubscriberData` | `map.isd.request` → `map.isd.response` |

The package also defines the `Message` JSON schema, parameter keys, status values and the
gateway's failure codes — the contract for consumers in other languages.

## Usage

```go
nc, err := nats.Connect(natsURL)
if err != nil { /* ... */ }
defer nc.Close()

gw, err := ss7client.New(nc)
if err != nil { /* ... */ }
defer gw.Close()

sri, err := gw.SendRoutingInfoForSM(ctx, ss7client.SRIRequest{
	MSISDN:               "905321112233",
	ServiceCentreAddress: ss7client.E164("905001112233"),
})
if err != nil { /* *ss7client.GatewayError, ErrInvalidRequest, or a context error */ }

_, err = gw.MTForwardSM(ctx, ss7client.MTRequest{
	IMSI:                 sri.IMSI,
	NetworkNodeNumber:    ss7client.E164(sri.NetworkNodeNumber),
	ServiceCentreAddress: ss7client.E164("905001112233"),
	TPDU:                 smsDeliverTPDU, // 3GPP TS 23.040, opaque to the gateway
})

sub, err := gw.SubscribeMO("smsc", func(m *ss7client.Message) {
	// m.Sender: originating MSC, m.Recipient: SMSC address, m.PayloadData: SMS-SUBMIT TPDU
})

loc, err := gw.AnyTimeInterrogation(ctx, ss7client.ATIRequest{
	MSISDN:    "905321112233",
	Requested: ss7client.RequestedInfo{Location: true, State: true},
})
// loc.Info.Location.CellGlobalID, loc.Info.Location.VLRNumber, loc.Info.State.State, …

route, err := gw.SendRoutingInfo(ctx, ss7client.RoutingRequest{MSISDN: "905321112233"})
// route.Routing.RoamingNumber.Digits (MSRN), route.Routing.Forwarding, route.Routing.VMSCAddress, route.IMSI

// USSD: a menu for whoever dials a code routed to "balance" (map.ussd_routes).
gw.SubscribeUSSD("balance", "balance-app", func(e *ss7client.USSDEvent) {
	if e.Action != ss7client.USSDBegin {
		return // an end: e.Status and e.Detail say how the session ended
	}
	go func() { // never wait for a human on the subscription's goroutine
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		a, err := gw.USSDRequest(ctx, e.SessionID, ss7client.USSDText{Text: "1 Balance\n2 Offers"})
		if err != nil {
			// The session may still be open: end it, on a fresh context.
			_ = gw.USSDReject(context.Background(), e.SessionID, ss7client.MAPErrSystemFailure)
			return
		}
		_ = gw.USSDRelease(ctx, e.SessionID, &ss7client.USSDText{Text: "You chose " + a.Reply.Text})
	}()
})

// Location updates: the application is the HLR's location management.
gw.ServeLocationUpdates("hlr", func(ctx context.Context, t *ss7client.LocationUpdateTurn) ss7client.LocationUpdateAnswer {
	switch t.Action {
	case ss7client.ULUpdateLocation: // a VLR registers t.IMSI
		profile, ok := subscribers.Lookup(ctx, t.IMSI)
		if !ok {
			return ss7client.LocationUpdateAnswer{Reject: ss7client.MAPErrUnknownSubscriber}
		}
		return ss7client.LocationUpdateAnswer{Insert: profile} // *ss7client.SubscriberData
	case ss7client.ULInsertResult: // t.Insert says what the VLR did not take, t.Status a failure
		if t.Status != "" {
			return ss7client.LocationUpdateAnswer{Reject: ss7client.MAPErrSystemFailure}
		}
		subscribers.Register(ctx, t.IMSI, t.Update.VLRNumber, t.Update.MSCNumber)
		return ss7client.LocationUpdateAnswer{Accept: true}
	}
	return ss7client.LocationUpdateAnswer{} // ULEnd: t.Status says how it ended
})
```

## Behaviour worth knowing

- **Correlation.** Responses are broadcast on the response subject; every client instance
  receives every response and keeps only those carrying its own correlation IDs.
- **Failures.** Every outcome comes back on the response subject as `*GatewayError`:
  - the peer answered with a MAP ReturnError → `Status` `REMOTE_ERROR`, `Code` the MAP error code
    (`MAPErrAbsentSubscriberSM`, `MAPErrUnknownSubscriber`, …), `Message` its name — the input to
    an SMSC's retry decision;
  - a TCAP Reject → `REMOTE_ERROR` with `ErrCodeRemoteError`;
  - the dialogue timed out or was aborted → `TIMEOUT` / `ABORTED` with a negative `Code`;
  - the gateway refused the request before starting a dialogue → `REJECTED` with
    `ErrCodeRejected` and the reason (gateway 0.7.4 and later).

  Any status the client does not recognise is treated as a failure, never as success.
- **Deadlines.** Gateways before 0.7.4 send no response for a request they reject before starting
  a dialogue, and no gateway can answer a request that is not valid JSON. The client checks the fields the gateway requires before publishing, and bounds a
  request without a context deadline by `DefaultTimeout` (40s, longer than the gateway's default
  30s TCAP dialogue timeout).
- **Access.** AnyTimeInterrogation, ProvideSubscriberInfo and SendRoutingInfo return the IMSI,
  serving node or location of any subscriber they are asked about; USSD puts text on any handset
  and reads what subscribers type.
  - Grant publish rights only to the applications that need them:
    - `map.ati.request`, `map.psi.request`, `map.routing.request`;
    - `map.ussd.start` (text on any handset, unasked);
    - `map.ussd.command` (acts on any live session whose ID it holds).

    - `map.cl.request`, `map.prn.request`, `map.isd.request` (cancel any subscriber's
      registration, take a roaming number to route their calls, change their profile in a VLR).

    The call routing subjects are outside `map.sri.*`, so granting SRI-for-SM does not grant them.
    A grant of `map.*.request` or `map.>` does.
  - Response and event subjects are broadcast. Subscribe rights show every answer, from every
    requester:
    - `map.ati.response`, `map.psi.response` and `map.routing.response` carry IMSIs, locations and
      roaming numbers;
    - `map.ussd.response` carries what subscribers type, PINs included;
    - `map.ussd.event.<route>` carries who dialled what.

    Grant each USSD route only to the application that serves it.
  - Whoever subscribes to `map.ul` answers location updates: it is the HLR. Grant it to the HLR
    application alone, with permission to publish its replies (NATS `allow_responses`); the
    gateway needs to subscribe to its reply inboxes (`_INBOX.>`). Grant publish on `map.ul` to
    the gateway alone: anyone who can publish there can send a forged turn and read the
    profile the HLR application answers with. `map.ul` and
    `map.isd.response` carry subscriber profiles, IMEISVs and the VLRs subscribers are in.
  - `WithDecodeErrorHandler` receives raw payloads, USSD text included.
  - A `Client` subscribes to an operation's response subject on its first request of that
    operation, so an account limited to some operations works.
- **MO delivery** is a plain NATS publish: an MO-ForwardSM published while no subscriber is
  connected is lost. Use a queue group to share the stream across instances.

## Licence

[Apache License 2.0](LICENSE). Copyright 2026 3eAI Labs Ltd. Contributions are accepted under the
[3eAI Labs Contributor Licence Agreement](CLA.md); see [CONTRIBUTING.md](CONTRIBUTING.md).
