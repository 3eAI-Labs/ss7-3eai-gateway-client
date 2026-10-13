# ss7-3eai-gateway-client

Go client and NATS wire contract for [ss7-3eai-gateway](https://github.com/3eAI-Labs/ss7-3eai-gateway),
an SS7 signalling gateway (M3UA / SCCP / TCAP / MAP) that applications drive over NATS.

Licensed under the Apache License 2.0, separately from the gateway (AGPL-3.0-only), so an SMSC or
any other application can link it under any licence.

```bash
go get github.com/3eAI-Labs/ss7-3eai-gateway-client
```

## What it covers

The interface the gateway implements end to end — the MAP short-message path:

| Operation | Method | Subjects |
|-----------|--------|----------|
| SendRoutingInfoForSM (MAP op 45) | `SendRoutingInfoForSM` | `map.sri.request` → `map.sri.response` |
| MT-ForwardSM (MAP op 44) | `MTForwardSM` | `map.mt.sms.request` → `map.mt.sms.response` |
| MO-ForwardSM (MAP op 46), received from a foreign MSC | `SubscribeMO` | `map.mo.sms` |

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
```

## Behaviour worth knowing

- **Correlation.** Responses are broadcast on the response subject; every client instance
  receives every response and keeps only those carrying its own correlation IDs.
- **Failures.** Every outcome comes back on the response subject as `*GatewayError`:
  - the peer answered with a MAP ReturnError → `Status` `REMOTE_ERROR`, `Code` the MAP error code
    (`MAPErrAbsentSubscriberSM`, `MAPErrUnknownSubscriber`, …), `Message` its name — the input to
    an SMSC's retry decision;
  - a TCAP Reject → `REMOTE_ERROR` with `ErrCodeRemoteError`;
  - the dialogue timed out or was aborted → `TIMEOUT` / `ABORTED` with a negative `Code`.

  Any status the client does not recognise is treated as a failure, never as success.
- **Deadlines.** The gateway sends no response for a request it rejects before starting a
  dialogue. The client checks the fields the gateway requires before publishing, and bounds a
  request without a context deadline by `DefaultTimeout` (40s, longer than the gateway's default
  30s TCAP dialogue timeout).
- **MO delivery** is a plain NATS publish: an MO-ForwardSM published while no subscriber is
  connected is lost. Use a queue group to share the stream across instances.

## Licence

[Apache License 2.0](LICENSE). Copyright 2026 3eAI Labs Ltd. Contributions are accepted under the
[3eAI Labs Contributor Licence Agreement](CLA.md); see [CONTRIBUTING.md](CONTRIBUTING.md).
