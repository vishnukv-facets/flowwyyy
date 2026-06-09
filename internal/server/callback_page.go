package server

import (
	"html"
	"net/http"
	"strings"
)

// Standalone result pages for OAuth / App-manifest callbacks.
//
// These render in the operator's browser on the PUBLIC ingress (and localhost),
// neither of which serves the SPA bundle — so the flow design system can't be
// linked, it has to be inlined. Everything here mirrors the real app: the
// tokens.css palette (indigo #645df6, brand dark surfaces), Plus Jakarta Sans +
// JetBrains Mono (loaded from Google Fonts, with system fallbacks), the dotted
// grid backdrop from .content, and the FlowMark logo. One renderer backs every
// callback page (GitHub + Slack) so they stay visually identical.

type callbackResultKind string

const (
	callbackOK    callbackResultKind = "ok"
	callbackError callbackResultKind = "error"
)

// flowMarkSVG is the static FlowMark (see components/FlowMark.tsx): the indigo
// gradient tile with a wave arch over two foot dots. Inlined so the brand reads
// the same on these standalone pages as in the app.
const flowMarkSVG = `<svg width="26" height="26" viewBox="0 0 36 36" aria-hidden="true">` +
	`<defs><linearGradient id="fm" x1="0" y1="0" x2="36" y2="36" gradientUnits="userSpaceOnUse">` +
	`<stop offset="0" stop-color="#645df6"/><stop offset="1" stop-color="#8b87f8"/></linearGradient></defs>` +
	`<rect width="36" height="36" rx="8" fill="url(#fm)"/>` +
	`<path d="M9 23 Q14 23 14 18 Q14 13 19 13 Q24 13 24 18 Q24 23 27 23" fill="none" stroke="#fff" stroke-width="2.6" stroke-linecap="round"/>` +
	`<circle cx="9" cy="23" r="2" fill="#fff"/><circle cx="27" cy="23" r="2" fill="#fff"/></svg>`

// callbackPageCSS holds the inlined styles. Kept as a raw const (not a format
// string) because it contains % units (50%, 100%) that would break fmt verbs.
const callbackPageCSS = `:root{
--bg:#0c0c0c;--bg-1:#111114;--border:#26262e;--border-strong:#34343f;
--text:#e7e7ea;--text-2:#aeaeb8;--text-3:#74747f;
--accent-hi:#8b87f8;--ok:#2eb672;--danger:#e25757;
--sans:'Plus Jakarta Sans',ui-sans-serif,system-ui,sans-serif;
--mono:'JetBrains Mono',ui-monospace,'SF Mono',Menlo,monospace}
*{box-sizing:border-box}html,body{height:100%}
body{margin:0;font-family:var(--sans);color:var(--text);background:var(--bg);
background-image:radial-gradient(rgba(255,255,255,0.022) 1px,transparent 1px);
background-size:22px 22px;background-position:-1px -1px;
display:grid;place-items:center;padding:24px;-webkit-font-smoothing:antialiased;text-rendering:optimizeLegibility}
.card{width:100%;max-width:29rem;background:var(--bg-1);border:1px solid var(--border);
border-radius:14px;box-shadow:0 22px 60px -14px rgba(0,0,0,0.72);padding:34px 32px;text-align:center}
.brand{display:inline-flex;align-items:center;gap:8px;margin-bottom:24px;
color:var(--text-2);font-weight:600;font-size:14px;letter-spacing:-.01em}
.brand svg{display:block}
.glyph{width:52px;height:52px;border-radius:50%;display:grid;place-items:center;
margin:0 auto 18px;font-size:24px;line-height:1;font-weight:700;border:1px solid var(--border-strong)}
.glyph.ok{color:var(--ok);background:rgba(46,182,114,.12);border-color:rgba(46,182,114,.38)}
.glyph.error{color:var(--danger);background:rgba(226,87,87,.12);border-color:rgba(226,87,87,.4)}
h1{margin:0 0 10px;font-size:1.2rem;font-weight:600;letter-spacing:-.01em;color:var(--text)}
.body{color:var(--text-2);line-height:1.55;font-size:14px}
.body a{color:var(--accent-hi);text-decoration:none;font-weight:500}
.body a:hover{text-decoration:underline}
.body code{font-family:var(--mono);font-size:12px;background:var(--bg);
border:1px solid var(--border);padding:1px 6px;border-radius:6px;color:var(--text)}
.hint{margin-top:18px;color:var(--text-3);font-size:12.5px}`

const callbackFontLinks = `<link rel="preconnect" href="https://fonts.googleapis.com">` +
	`<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>` +
	`<link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;500&family=Plus+Jakarta+Sans:wght@400;500;600;700&display=swap" rel="stylesheet">`

// writeCallbackResultHTML renders the branded standalone result page. bodyHTML
// is trusted markup (callers escape any dynamic text they embed); title is
// escaped here. status defaults to 200.
func writeCallbackResultHTML(w http.ResponseWriter, status int, kind callbackResultKind, title, bodyHTML string) {
	if status == 0 {
		status = http.StatusOK
	}
	glyph := "✓"
	if kind == callbackError {
		glyph = "✕"
	}
	titleEsc := html.EscapeString(title)

	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="en" data-theme="dark"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1"><title>`)
	b.WriteString(titleEsc)
	b.WriteString(` · flow</title>`)
	b.WriteString(callbackFontLinks)
	b.WriteString(`<style>`)
	b.WriteString(callbackPageCSS)
	b.WriteString(`</style></head><body><main class="card"><div class="brand">`)
	b.WriteString(flowMarkSVG)
	b.WriteString(` flow</div><div class="glyph `)
	b.WriteString(string(kind))
	b.WriteString(`">`)
	b.WriteString(glyph)
	b.WriteString(`</div><h1>`)
	b.WriteString(titleEsc)
	b.WriteString(`</h1><div class="body">`)
	b.WriteString(bodyHTML)
	b.WriteString(`</div><p class="hint">You can close this tab and return to Mission Control.</p></main></body></html>`)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(b.String()))
}
