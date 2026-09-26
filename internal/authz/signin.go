package authz

import (
	"html/template"
	"net/http"
	"net/url"
)

// signInPage is what the sign-in form shows. The client's name is its own
// choice and proves nothing, so the page leads with where the person will be
// sent afterwards - which the allowlist does vouch for - as the MCP
// specification asks a consent screen to.
type signInPage struct {
	Realm       string
	Client      string
	Destination string
	Loopback    bool
	Action      string
	Request     string
	Username    string
	Error       string
}

func newSignInPage(zone *Zone, client clientClaims, redirect, request string) signInPage {
	page := signInPage{
		Realm:   zone.Realm,
		Client:  client.Name,
		Action:  AuthorizePath,
		Request: request,
	}
	if u, err := url.Parse(redirect); err == nil {
		page.Destination = u.Host
		page.Loopback = u.Scheme == "http" && isLoopbackHost(u.Hostname())
	}
	if page.Client == "" {
		page.Client = "An application"
	}
	return page
}

// renderSignIn writes the page with headers that keep it out of frames and
// caches. There is no form-action directive on purpose: browsers apply it to
// the redirect that follows a submitted form, and that redirect goes to the
// client, wherever it is.
func renderSignIn(w http.ResponseWriter, status int, page signInPage) {
	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'")
	header.Set("X-Frame-Options", "DENY")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Robots-Tag", "noindex, nofollow")
	w.WriteHeader(status)
	_ = signInTemplate.Execute(w, page)
}

var signInTemplate = template.Must(template.New("signin").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in - {{.Realm}}</title>
<style>
:root {
  color-scheme: light dark;
  --bg: #f6f7f9; --card: #ffffff; --text: #1d2330; --muted: #5b6474;
  --line: #d8dce3; --accent: #2458d6; --accent-text: #ffffff;
  --warn-bg: #fff6e0; --warn-text: #6b4a00; --error-bg: #fdecec; --error-text: #9b1c1c;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #14171c; --card: #1d2128; --text: #e6e9ef; --muted: #9aa3b2;
    --line: #333a45; --accent: #6d95f5; --accent-text: #0d1117;
    --warn-bg: #3a2f12; --warn-text: #f3d58a; --error-bg: #3b1b1b; --error-text: #f5b4b4;
  }
}
* { box-sizing: border-box; }
body {
  margin: 0; min-height: 100vh; display: flex; align-items: center; justify-content: center;
  padding: 16px; background: var(--bg); color: var(--text);
  font: 16px/1.5 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
}
main {
  width: 100%; max-width: 400px; background: var(--card); border: 1px solid var(--line);
  border-radius: 12px; padding: 28px 24px;
}
h1 { font-size: 1.25rem; margin: 0 0 16px; }
p { margin: 0 0 12px; }
.muted { color: var(--muted); font-size: 0.95rem; }
.note { padding: 10px 12px; border-radius: 8px; font-size: 0.9rem; }
.warn { background: var(--warn-bg); color: var(--warn-text); }
.error { background: var(--error-bg); color: var(--error-text); }
form { margin-top: 20px; }
label { display: block; font-size: 0.9rem; font-weight: 600; margin: 0 0 14px; }
input[type=text], input[type=password] {
  display: block; width: 100%; margin-top: 6px; padding: 10px 12px; font: inherit;
  color: var(--text); background: var(--bg); border: 1px solid var(--line); border-radius: 8px;
}
/* Sign in comes first in the markup, because Enter submits with the first
   button; it is drawn last, where a primary button is expected. */
.actions { display: flex; flex-direction: row-reverse; gap: 10px; margin-top: 20px; }
button {
  flex: 1; padding: 10px 14px; font: inherit; font-weight: 600; border-radius: 8px; cursor: pointer;
  border: 1px solid var(--accent); background: var(--accent); color: var(--accent-text);
}
button.secondary { background: transparent; color: var(--text); border-color: var(--line); }
</style>
</head>
<body>
<main>
<h1>{{.Realm}}</h1>
{{- if .Request}}
<p><strong>{{.Client}}</strong> is asking to read this documentation as you.</p>
<p class="muted">After you sign in you will be sent to <strong>{{.Destination}}</strong>.</p>
{{- if .Loopback}}
<p class="note warn">That address is a program on this computer. Continue only if you started this sign-in yourself, from a program you trust.</p>
{{- end}}
{{- end}}
{{- if .Error}}
<p class="note error" role="alert">{{.Error}}</p>
{{- end}}
{{- if .Request}}
<form method="post" action="{{.Action}}">
<input type="hidden" name="request" value="{{.Request}}">
<label>Name
<input type="text" name="username" value="{{.Username}}" autocomplete="username" autocapitalize="none" spellcheck="false" required autofocus>
</label>
<label>Password
<input type="password" name="password" autocomplete="current-password" required>
</label>
<div class="actions">
<button type="submit" name="decision" value="allow">Sign in</button>
<button type="submit" name="decision" value="deny" class="secondary" formnovalidate>Cancel</button>
</div>
</form>
{{- end}}
</main>
</body>
</html>
`))
