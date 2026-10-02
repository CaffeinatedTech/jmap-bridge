package httpapi

import "net/http"

// The OAuth consent screen requires an "application home page" and a
// "privacy policy" link that are publicly reachable on a domain the
// operator controls (FR-D.13). Serving them from the bridge puts both on
// the same origin as the OAuth callback, so the two links and the
// authorized domain are all this one origin. Like /oauth, they are
// unauthenticated — a browser reaches them before any client exists — and
// they carry no account information.

// handleHome serves GET / (FR-D.13): the consent screen's application home
// page.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	writeHTML(w, homePageHTML)
}

// handlePrivacy serves GET /privacy (FR-D.13): the privacy policy the
// consent screen links to.
func (s *Server) handlePrivacy(w http.ResponseWriter, r *http.Request) {
	writeHTML(w, privacyPageHTML)
}

func writeHTML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(body))
}

// homePageHTML is deliberately static — no scripts, no account data — so a
// public, unauthenticated page can never leak a configured mailbox.
const homePageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>jmap-bridge</title>
</head>
<body>
<h1>jmap-bridge</h1>
<p>jmap-bridge is self-hosted software that runs a JMAP server in front of an
ordinary email account, reading and sending mail over IMAP and SMTP (and,
optionally, contacts over CardDAV) so that JMAP-only clients can use that
mailbox.</p>
<p>This instance is operated by the person who deployed it and serves only the
mail accounts they configured. The JMAP API requires the account's access
token; these informational pages exist for the Google OAuth consent screen.</p>
<ul>
<li><a href="/privacy">Privacy policy</a></li>
<li><a href="https://github.com/CaffeinatedTech/jmap-bridge">Source code</a></li>
</ul>
</body>
</html>
`

// privacyPageHTML discloses how the bridge accesses, uses, stores and
// shares Google user data, as Google's consent-screen and API Services User
// Data Policy require. The operator of an instance is its data controller;
// this text describes what the software does.
const privacyPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>jmap-bridge privacy policy</title>
</head>
<body>
<h1>Privacy policy</h1>
<p>jmap-bridge is self-hosted software. The operator who deployed this
instance is the controller of any data it holds; this page describes what the
software does with Google user data when the operator connects a Google
account.</p>

<h2>What is accessed</h2>
<ul>
<li>With the account holder's consent, the bridge accesses that Google
account's mail through the <code>https://mail.google.com/</code> scope: reading
mailboxes, messages, flags and labels, and sending mail through Gmail's SMTP
servers.</li>
<li>If contacts are configured, it additionally requests
<code>https://www.googleapis.com/auth/carddav</code> to read and write the
account's contacts.</li>
<li>No other Google data is requested.</li>
</ul>

<h2>How it is used</h2>
<p>Solely to provide the mailbox and contacts to the JMAP client(s) the
operator configures. The data is not used for advertising, profiling, or
training machine-learning models, and its use complies with the Google API
Services User Data Policy, including the Limited Use requirements.</p>

<h2>Where it is stored</h2>
<p>Message headers, flags, mailbox structure, a local search index and cached
message bodies are stored on the operator's own server, in the configured data
directory. OAuth refresh and access tokens are stored there as well, encrypted
with <code>JMAP_BRIDGE_SECRET_KEY</code> when one is configured.</p>

<h2>Sharing</h2>
<p>Data is not sold, shared, or transferred to third parties. The software
contains no analytics or telemetry, and the API is reachable only with the
account's access token.</p>

<h2>Retention and deletion</h2>
<p>The operator controls retention: deleting the data directory removes the
local copy. Revoking the app's access at
<a href="https://myaccount.google.com/permissions">myaccount.google.com/permissions</a>
stops all further access.</p>

<h2>Contact</h2>
<p>For questions about this instance, contact its operator. The project source
and issue tracker are at
<a href="https://github.com/CaffeinatedTech/jmap-bridge">github.com/CaffeinatedTech/jmap-bridge</a>.</p>
</body>
</html>
`
