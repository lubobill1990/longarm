package signin

// Summary is one line naming exactly what the sign-in enrolled (as
// StepDone's message), e.g. "Signed in as alice@contoso.com: client device
// alice-box, worker box (user session), admin worker box-admin (runs as
// SYSTEM, yours only)". The tray shows it, `longarm login` and `longarmd
// login` print it.
func (r *Result) Summary() string { return r.summary() }
