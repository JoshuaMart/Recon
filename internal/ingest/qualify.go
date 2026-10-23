package ingest

// Outcomes, as stored on observations: ok the target is there, fail the target
// answered with an absence (the only kind that can lead to death), error
// nothing conclusive was obtained.
const (
	OK    = "ok"
	Fail  = "fail"
	Error = "error"
)

// qualifyDNS derives a host's dns outcome and current dns_state. observe is
// false for a host the resolve stage never reached: silence is not a
// measurement, so it writes nothing.
func qualifyDNS(h Host, degraded bool) (outcome, state string, observe bool) {
	switch h.Status {
	case "live":
		return OK, "resolving", true
	case "wildcard":
		return OK, "wildcard", true
	case "dead":
		switch h.Reason {
		case "nxdomain":
			// A resolver pool that was not validated can hijack nxdomain, so a
			// degraded run cannot conclude that a name is gone.
			if degraded {
				return Error, "nxdomain", true
			}
			return Fail, "nxdomain", true
		case "no_answer":
			// The name exists without an address: a mail-only host is not dead.
			return OK, "nodata", true
		}
		return Error, "unknown", true
	}
	return "", "", false
}

// qualifyTCP derives a host's tcp outcome from what the sweep attempted.
func qualifyTCP(d TCPData) (outcome, state string) {
	switch {
	case d.Open > 0:
		return OK, "open"
	case d.Scanned > 0 && d.Refused == d.Scanned:
		// Every port refused: the host answers and nothing listens.
		return Fail, "closed"
	case d.Filtered > 0:
		// Timeouts are indistinguishable from filtering or a ban.
		return Error, "filtered"
	}
	return Error, "unknown"
}
