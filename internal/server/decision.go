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
func setDecisionHeader(w http.ResponseWriter, def *provider.Def, acct *provider.Account, model string, attempts int) {
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
	w.Header().Set(DecisionHeader, b.String())
}
