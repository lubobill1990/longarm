# Admin worker

## Pairing an admin worker with a worker enrolled after it

The hub records an admin worker and its machine's worker as companions only
when the admin worker enrolls (internal/hub/link.go pairAdminWorker: the
worker the same approval enrolled, else the owner's one live worker on that
host without a companion). When the worker is replaced later - its owner
signs it in again, or removes it and enrolls a new one - the admin worker
keeps no companion: `longarm ls` and the tray show two machines, and
nothing pairs them again short of `longarmd admin-worker disable` and a new
admin worker sign-in. Pair at the worker's enrollment too (the same rule
from the worker's side: an unpaired admin worker on its host, same owner),
in enrollOwnedWorker / the link approval.

## The admin worker's name on the /link page

`longarmd login` and the tray leave the admin worker's name to the hub, so
the confirmation page shows `(default: <worker name>-admin)` instead of
the name it will get. Send signin.Options.AdminWorkerName (the worker's
name + "-admin" when the worker's is given) so the page names it exactly.
