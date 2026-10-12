package server

import (
	"net/http"
	"strconv"
	"strings"

	"onegw/internal/provider"
)

// DecisionHeader names the response header that self-documents which
// provider/account/model served a request (modeled on OmniRoute's
// X-OmniRoute-Decision). Zero config: every proxied response carries it.
const DecisionHeader = "X-OneGW-Decision"

// setDecisionHeader stamps w with the routing decision for the (def, acct,
// model) about to serve, using the per-request attempt counter the Caller
// owns. Call it in the Caller immediately BEFORE the upstream call — before
// any body write commits headers — so on combo fallback the eventually
// successful attempt's values are the ones the client sees (a failed
// attempt that already wrote a body leaves its own header; that is
// truthful). Never sent upstream: it is set on the client's response
// writer only.
//
// request=<the client's own model string> and fallback=<true|false> were
// added for clients that must TELL the user when their request was answered
// by something other than what they asked for. fallback is true exactly when
// the serving target is not the one the client's model string resolves to
// first: a combo whose first leg refused, or a route whose first target was
// filtered out. attempts= cannot say this on its own — a retry onto the SAME
// target is attempts=2 with fallback=false, and an account rotation inside
// one provider is not a different model at all. d may be nil (a caller with
// no decision record): request= is then omitted and fallback=false, which
// keeps those responses byte-identical to the pre-existing fields.
func setDecisionHeader(w http.ResponseWriter, d *delivery, def *provider.Def, acct *provider.Account, model string, attempts int) {
	var b strings.Builder
	b.WriteString("provider=")
	b.WriteString(def.Name)
	if acct != nil && acct.Name != "" {
		b.WriteString("; account=")
		b.WriteString(acct.Name)
	}
	b.WriteString("; model=")
	b.WriteString(model)
	b.WriteString("; attempts=")
	b.WriteString(strconv.Itoa(attempts))
	if req := d.requestedModel(); req != "" {
		b.WriteString("; request=")
		b.WriteString(req)
	}
	b.WriteString("; fallback=")
	b.WriteString(strconv.FormatBool(d.servedFallback(def.Name, model)))
	w.Header().Set(DecisionHeader, b.String())
}
