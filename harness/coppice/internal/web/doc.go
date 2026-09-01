// Package web serves the phone. It holds a Dialer that is the only way
// anything in this package speaks to the coppice socket, a bearer token
// with a ban list for the guesser, and the four ways this box can get a
// TLS certificate a phone will trust, including a local CA the phone
// installs once. It also serves four JSON paths under /api behind the
// same guard, and answers a gate ask by writing the file the gate reads.
// The installable PWA itself is served at / with no token, since the shell
// has to load before the operator has one to send.
//
// The socket coppice-server listens on is bound to the operator's uid and
// nothing else can reach it. HTTPS has no such wall, so this package adds
// two of its own: a bearer token on every request, and TLS from a
// certificate the phone already trusts. The tailnet or the LAN is the outer
// wall. Both, not either.
//
// Nothing here imports the coppice server package directly. Everything
// upstream goes through Dialer, so the whole package is testable against a
// fake JSONL server, and a phone client is exactly the kind of client a
// terminal is.
package web
