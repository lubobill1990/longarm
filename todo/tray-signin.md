# The tray's sign-in state

## An admin worker the hub refuses still looks signed in

The tray tells a refused client or worker certificate from a valid one
(HubLink.refused from the daemons' local APIs), but it can't reach the
admin worker's local API (SYSTEM's data dir, an owner-only pipe), so an
admin worker whose certificate the hub revoked shows "running" and
counts as signed in (longarmd.AdminWorkerStatus: SignedIn is "not
pending and the service runs"), and nothing asks for its sign-in again.
Start: the admin worker writes its link state (connected, refused) into
its hand-off dir next to error.txt (internal/longarmd/adminworker.go);
AdminWorkerStatus reads it; a refusal re-runs the sign-in request
(AdminRequestFile) so the tray's next sign-in carries it.

## Expiry of each part, not of the client certificate only

Account.Expires / Expired follow the client certificate, else the
worker's (internal/tray/signin.go account()): with a valid client and an
expired worker the card says "Sign in" without saying why, and the
"Signed in as" user is the client certificate's even when the worker's
owner differs. Give each part of Account.Need its own expiry and the
worker's owner when it differs, and show them in AccountCard / SignInCard
(web/src/tray/AccountTab.tsx).

## A refused certificate reconnects on the fast backoff

"remote error: tls: bad certificate" arrives as codes.Unavailable, which
hubclient.Unreachable treats as a network failure: longarmd and the
client daemon retry it on the fast backoff although only a sign-in can
help. hubclient.CertRefused (internal/hubclient/refused.go) recognizes
it; Reconnect.Next could back off to its maximum for it
(internal/hubclient/reconnect.go), and `longarm doctor`'s hubFix
(cmd/longarm/doctor.go) could use the same test instead of its keyword
list.
