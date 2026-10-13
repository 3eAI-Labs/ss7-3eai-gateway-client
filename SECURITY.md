# Security Policy

## Supported versions

Only the most recent release receives fixes.

## Reporting a vulnerability

**Do not open a public issue.**

Use GitHub's private vulnerability reporting:
[**Report a vulnerability**](https://github.com/3eAI-Labs/ss7-3eai-gateway-client/security/advisories/new).
The report stays private between you and the maintainers until a fix is published.

Include the module version, what an attacker gains, and a reproduction — ideally a failing test.
If the problem is in the gateway rather than in this client, report it to
[ss7-3eai-gateway](https://github.com/3eAI-Labs/ss7-3eai-gateway/security/advisories/new) instead.

## Scope

In scope: a response delivered to the wrong caller (correlation ID confusion between concurrent
requests or client instances), a request reported as successful when the gateway reported a
failure, subscriber data (IMSI, MSISDN, SMS content) written somewhere the caller did not ask for,
and resource exhaustion in the client triggered by messages on its subjects.

Out of scope: the NATS server and the transport security of the connection you pass in — the
client uses the connection exactly as you configured it.
